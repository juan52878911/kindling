package aigw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/chispa"
	"github.com/juan52878911/kindling/pkg/scheduler"
	"github.com/juan52878911/kindling/pkg/von"
)

// LabelGateway marca las máquinas de un gateway de IA. Solo adopta (y congela
// al arrancar y al parar) las que la llevan con su id: un humano con `kling run
// -from von-smol` o el gateway MCP en el mismo daemon no se ven afectados.
const LabelGateway = "ai.gateway"

// Options configura el gateway. Los ceros tienen valores por defecto
// razonables para un host con poca memoria (ver withDefaults).
type Options struct {
	Client     *api.Client // el daemon
	ConfigPath string      // ai.json; vacío si se da Config
	Config     *Config
	// Replicas sustituye a pkg/scheduler (tests).
	Replicas Replicas

	ID           string        // valor de LabelGateway; "default"
	NamePrefix   string        // prefijo de las máquinas; "gw-"
	Idle         time.Duration // sin peticiones antes de congelar una réplica; 2 min
	MaxReplicas  int           // por modelo, salvo max_replicas en el registro; 2
	MaxInflight  int           // peticiones por réplica antes de pedir otra; 1
	KeepWarm     int           // N modelos populares siempre despiertos; 0
	ChispaBudget int64         // bytes de modelos Chispa cargados; 256 MiB
	VONTimeout   time.Duration // plazo de una escalada; 60 s
	// EncoderTimeout es el plazo de la capa 3 de una tarea de domótica,
	// despertar la réplica incluido; 5 s. Pasado, la decisión escala a VON.
	EncoderTimeout time.Duration
	ProxyTimeout   time.Duration // plazo de una petición OpenAI; 5 min
	MaxBody        int64         // cuerpo de una petición; 1 MiB
	MaxProxyBytes  int64         // respuesta de una réplica por el proxy; 32 MiB
	PopularityFile string        // "" = solo en memoria
	// EvalDir guarda los registros de `kling ai eval` (uno por tarea). Vacío =
	// ai-evals/ junto al registro; sin registro en disco, ninguno (y ninguna
	// cascada respaldada).
	EvalDir string
}

func (o *Options) withDefaults() {
	if o.ID == "" {
		o.ID = "default"
	}
	if o.NamePrefix == "" {
		o.NamePrefix = "gw-"
	}
	if o.Idle <= 0 {
		o.Idle = 2 * time.Minute
	}
	if o.MaxReplicas <= 0 {
		o.MaxReplicas = 2
	}
	if o.MaxInflight <= 0 {
		o.MaxInflight = 1
	}
	if o.ChispaBudget == 0 {
		o.ChispaBudget = 256 << 20
	}
	if o.VONTimeout <= 0 {
		o.VONTimeout = 60 * time.Second
	}
	if o.EncoderTimeout <= 0 {
		o.EncoderTimeout = 5 * time.Second
	}
	if o.ProxyTimeout <= 0 {
		o.ProxyTimeout = 5 * time.Minute
	}
	if o.MaxBody <= 0 {
		o.MaxBody = 1 << 20
	}
	if o.MaxProxyBytes <= 0 {
		o.MaxProxyBytes = 32 << 20
	}
	if o.EvalDir == "" && o.ConfigPath != "" {
		o.EvalDir = filepath.Join(filepath.Dir(o.ConfigPath), "ai-evals")
	}
}

// Gateway es el gateway de IA.
type Gateway struct {
	opts     Options
	sched    *scheduler.Scheduler
	replicas Replicas
	chispa   *chispaCache
	domo     domoFiles // .chispas y .jenc de las tareas de domótica
	met      *metrics

	cfgMu    sync.RWMutex
	cfg      *Config
	rings    map[string]*ring        // tarea -> muestras; bajo cfgMu
	cascades map[string]CascadeState // tarea -> cascada activa o no, y por qué; bajo cfgMu

	auditSem chan struct{} // auditorías en vuelo (una): nunca compiten en masa con el tráfico
	calMu    sync.Mutex    // una recalibración a la vez

	repMu    sync.Mutex // caché de réplicas por modelo para /metrics
	repAt    time.Time
	repCache map[string]replicaCount

	// deploy consulta ChispaDeployAnnotation (el registro de `kling chispa deploy`):
	// la fuente de verdad de las etiquetas de un modelo chispa backend microvm, ya
	// que el invitado no es de fiar (chispaguest.go). deployMu/deployCache lo
	// cachean deployCacheTTL para no preguntar al daemon en cada clasificación.
	deploy      chispaDeployLookup
	deployMu    sync.Mutex
	deployCache map[string]deployCacheEntry
}

// New crea el gateway. No habla con el daemon hasta Start o la primera
// petición.
func New(o Options) (*Gateway, error) {
	o.withDefaults()
	cfg := o.Config
	if cfg == nil {
		var err error
		if cfg, err = LoadConfig(o.ConfigPath); err != nil {
			return nil, err
		}
	}
	g := &Gateway{
		opts: o, chispa: newChispaCache(o.ChispaBudget), met: newMetrics(), auditSem: make(chan struct{}, 1),
		deploy: clientDeployLookup{o.Client}, deployCache: map[string]deployCacheEntry{},
	}
	g.setConfig(cfg)

	s := scheduler.New(o.Client, o.Idle, false, 0)
	s.Port = von.Port
	s.MachineLabels = map[string]string{LabelGateway: o.ID}
	s.NamePrefix = o.NamePrefix
	// Sin TTL de red de seguridad hasta saber si el daemon sabe renovarlo
	// (Start): sin la capacidad "renew" lo cuenta desde la creación y un thaw
	// no lo reinicia, así que una réplica vieja se re-congelaría bajo tráfico.
	// Con ella, el planificador lo renueva y el TTL pasa a ser un
	// arrendamiento: si el gateway muere, sus réplicas se congelan solas.
	s.MachineTTL = -1
	s.MaxInflight = o.MaxInflight
	s.MaxReplicas = o.MaxReplicas
	s.KeepWarm = o.KeepWarm
	s.SetPopularityFile(o.PopularityFile)
	s.MaxReplicasFor = func(snap string) int {
		if _, m := g.config().replicaModel(snap); m != nil {
			return m.MaxReplicas
		}
		return 0
	}
	// El keepwarm recorre TODOS los snapshots del daemon: solo los dorados
	// registrados como modelos VON son de este gateway.
	s.Skip = func(sn *api.Snapshot) bool {
		_, m := g.config().replicaModel(sn.Name)
		return m == nil
	}
	s.OnAcquire = func(snap, how string, d time.Duration) {
		name, _ := g.config().replicaModel(snap)
		if name == "" {
			name = snap
		}
		g.met.wake(name, how, d)
		log.Printf("von %s: replica ready (%s) in %s", name, how, d.Round(time.Millisecond))
	}
	if len(cfg.Tenants) > 0 {
		ts := make([]scheduler.TenantLimit, len(cfg.Tenants))
		for i, t := range cfg.Tenants {
			ts[i] = scheduler.TenantLimit{Name: t.Name, Token: t.Token, MaxInflight: t.MaxInflight, MaxInstances: t.MaxInstances}
		}
		s.SetTenants(ts)
	}
	g.sched = s
	g.replicas = o.Replicas
	if g.replicas == nil {
		g.replicas = schedReplicas{s}
	}
	return g, nil
}

func (g *Gateway) config() *Config {
	g.cfgMu.RLock()
	defer g.cfgMu.RUnlock()
	return g.cfg
}

// setConfig instala un registro y decide qué cascadas se activan. Las muestras
// de las tareas que siguen existiendo se conservan (se copian si cambió el
// tamaño del anillo). Devuelve las notas de las cascadas (activas, forzadas,
// rechazadas).
func (g *Gateway) setConfig(c *Config) []string {
	st := g.gates(c) // lee ficheros: fuera del candado
	g.cfgMu.Lock()
	defer g.cfgMu.Unlock()
	old := g.rings
	g.rings = map[string]*ring{}
	for name, t := range c.Tasks {
		n := t.Samples
		if n == 0 {
			n = 2000
		}
		r := old[name]
		if r == nil || len(r.buf) != n {
			nr := newRing(n)
			if r != nil {
				for _, s := range r.snapshot() {
					nr.add(s)
				}
			}
			r = nr
		}
		g.rings[name] = r
	}
	g.cfg = c
	g.cascades = st
	return CascadeNotes(st)
}

// Reload relee el registro del disco. Los modelos Chispa se vuelven a leer en su
// siguiente uso (el fichero pudo cambiar). Devuelve las notas de las cascadas:
// quien activa escalate_to sin una evaluación que la respalde se entera aquí.
func (g *Gateway) Reload() ([]string, error) {
	if g.opts.ConfigPath == "" {
		return nil, errors.New("no config file to reload")
	}
	c, err := LoadConfig(g.opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	old := g.config()
	for _, m := range old.Models {
		if m.Kind == KindChispa {
			g.chispa.drop(m.Path)
		}
	}
	g.domo.reset()
	return g.setConfig(c), nil
}

// Start pone en marcha el segador (congela réplicas ociosas, mantiene el
// keepwarm) y congela lo que un gateway anterior con el mismo id dejara
// corriendo sin nadie que lo vigile. Vuelve enseguida; el segador para con ctx.
//
// El daemon solo hace falta para los modelos VON y embed (una réplica en una
// microVM): un registro que solo tiene Chispa nunca lo marca ni lo llama, así que
// arranca y sirve igual sin daemon, sin KVM ni sin vz. Si el registro SÍ trae
// un modelo VON o embed pero el daemon no contesta, se avisa una vez, con
// claridad, y el gateway sigue: las tareas Chispa siguen funcionando, las que
// necesitan VON fallarán hasta que el daemon esté.
func (g *Gateway) Start(ctx context.Context) {
	if g.opts.Client == nil {
		return
	}
	if g.config().NeedsDaemon() {
		if info, err := g.opts.Client.Info(ctx); err != nil {
			log.Printf("warning: no chispa-only registry: this one has a von or embed model, but the daemon at %s is not reachable (%v); chispa tasks work fine, but any task with von, escalate_to or a domotica encoder will fail until the daemon is up (kling daemon) and reachable (-host)",
				g.opts.Client.Endpoint(), err)
		} else {
			if info.Has("renew") {
				g.sched.MachineTTL = 0 // 2 × idle, renovado mientras el gateway viva
			}
			if n := g.freezeOwn(ctx); n > 0 {
				log.Printf("froze %d replica(s) left running by a previous gateway %q", n, g.opts.ID)
			}
		}
	}
	// El segador no llama al daemon si no hay ninguna réplica VON/embed
	// registrada (reapOnce solo toca lo que el planificador llegó a
	// despertar), así que dejarlo corriendo no exige un daemon presente: si
	// uno aparece más tarde (kling ai reload con un modelo VON nuevo), ya
	// está listo.
	go g.sched.Reap(ctx)
}

// Close congela las réplicas de este gateway que sigan corriendo: al salir no
// queda nada gastando CPU ni RAM, y la próxima vez vuelven con un thaw. Sin
// modelos VON/embed en el registro, o sin daemon, no hay nada que congelar y
// no se intenta hablar con él.
func (g *Gateway) Close(ctx context.Context) {
	if g.opts.Client == nil || !g.config().NeedsDaemon() {
		return
	}
	if n := g.freezeOwn(ctx); n > 0 {
		log.Printf("froze %d replica(s) on shutdown", n)
	}
}

func (g *Gateway) freezeOwn(ctx context.Context) int {
	ms, err := g.opts.Client.List(ctx)
	if err != nil {
		log.Printf("listing machines: %v", err)
		return 0
	}
	n := 0
	for _, m := range ms {
		if m.Labels[LabelGateway] != g.opts.ID || m.State != api.StateRunning {
			continue
		}
		if _, err := g.opts.Client.Freeze(ctx, m.ID); err != nil {
			log.Printf("freezing %s: %v", m.Name, err)
			continue
		}
		n++
	}
	return n
}

// ---- clasificación (Chispa, y la cascada si está respaldada)

// ClassifyRequest es el cuerpo de /v1/classify y /v1/decide.
type ClassifyRequest struct {
	Task   string         `json:"task"`
	Text   string         `json:"text"`
	Fields map[string]any `json:"fields,omitempty"`
	// Mode: "cascade" (por defecto: Chispa, y VON en lo que duda si la cascada de
	// la tarea está activa) o "chispa" (solo Chispa, aunque dude y aunque la cascada
	// esté activa).
	Mode string `json:"mode,omitempty"`
	// Explain añade la evidencia también a las respuestas confiadas de Chispa
	// (cuesta reservas de memoria; en las escaladas va siempre).
	Explain bool `json:"explain,omitempty"`
	// Lang es el idioma de una orden de domótica (es, en, auto).
	Lang string `json:"lang,omitempty"`
}

// ClassifyResponse es la respuesta.
type ClassifyResponse struct {
	Task     string `json:"task"`
	Label    string `json:"label"`
	Decision string `json:"decision,omitempty"` // /v1/decide: la misma etiqueta
	// Escalate: Chispa no llegó a su umbral. Con la cascada activa la etiqueta es
	// la de VON (source von); si no, es la de Chispa y quien llama decide qué
	// hacer con la duda. Nunca se pregunta a VON a escondidas.
	Escalate  bool              `json:"escalate,omitempty"`
	Prob      float64           `json:"prob"`   // probabilidad calibrada de Chispa para Label (0 si es unknown)
	Source    string            `json:"source"` // chispa | von
	LatencyMS float64           `json:"latency_ms"`
	Evidence  []chispa.Evidence `json:"evidence,omitempty"`
	Chispa    *ChispaAnswer     `json:"chispa,omitempty"`
	VON       *VONAnswer        `json:"von,omitempty"`
	Degraded  string            `json:"degraded,omitempty"` // la cascada estaba activa y VON no contestó
}

// ChispaAnswer es lo que dijo Chispa, conteste él o no.
type ChispaAnswer struct {
	Label      string             `json:"label"`
	Prob       float64            `json:"prob"`
	Threshold  float64            `json:"threshold"`
	Decision   string             `json:"decision"`
	Candidates []chispa.ClassProb `json:"candidates,omitempty"`
}

// VONAnswer es lo que dijo VON.
type VONAnswer struct {
	Model     string  `json:"model"`
	Answer    string  `json:"answer"`
	LatencyMS float64 `json:"latency_ms"`
}

// StatusError es un error con el código HTTP que le corresponde.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string { return e.Msg }

func statusf(code int, format string, a ...any) error {
	return &StatusError{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// maxText acota el texto a clasificar. Chispa lo recorta a su max_text de todas
// formas; esto evita guardar y copiar megas por petición.
const maxText = 64 << 10

// chispaDecide es la predicción de Chispa con los umbrales de la tarea: la etiqueta,
// su umbral efectivo y si contesta confiado. full pide la distribución y la
// evidencia (hace falta para escalar o explicar).
func chispaDecide(m *chispa.Model, tc *TaskConfig, in chispa.Input, full bool) (chispa.Prediction, float64, bool) {
	var p chispa.Prediction
	if full {
		p = m.PredictFull(in, 5)
	} else {
		p = m.Predict(in)
	}
	tau := p.Threshold
	if v, ok := tc.Thresholds[p.Label]; ok {
		tau = v
	}
	return p, tau, p.Prob >= tau
}

// escalation arma la pregunta a VON de una escalada: con top_k, VON solo elige
// entre los candidatos de Chispa. p tiene que traer la distribución (PredictFull),
// y labels todas las etiquetas del modelo (chispa.Model.Labels en proceso; en una
// tarea con backend microvm, las del registro de `kling chispa deploy`, no las
// que mande el invitado: ver chispaguest.go).
func (g *Gateway) escalation(tc *TaskConfig, labels []string, in chispa.Input, p chispa.Prediction) ([]string, chatReq) {
	allowed := labels
	if tc.TopK > 0 && tc.TopK < len(allowed) {
		allowed = make([]string, 0, tc.TopK)
		for _, c := range topN(p.Probs, tc.TopK) {
			allowed = append(allowed, c.Label)
		}
	}
	return allowed, g.chatFor(tc, allowed, in, p.Probs)
}

// Classify es la decisión de Chispa y, si la cascada de la tarea está activa, la
// escalada a VON de lo que duda.
func (g *Gateway) Classify(ctx context.Context, endpoint string, req ClassifyRequest) (*ClassifyResponse, error) {
	t0 := time.Now()
	cfg := g.config()
	tc := cfg.Tasks[req.Task]
	if tc == nil {
		return nil, statusf(http.StatusNotFound, "unknown task %q", req.Task)
	}
	if tc.Domotica != nil {
		return nil, statusf(http.StatusBadRequest, "task %q is a domotica task: use POST /v1/decide", req.Task)
	}
	if tc.Chispa == "" {
		return nil, statusf(http.StatusBadRequest, "task %q is a generation task: use POST /v1/generate", req.Task)
	}
	if len(req.Text) > maxText {
		return nil, statusf(http.StatusRequestEntityTooLarge, "text larger than %d bytes", maxText)
	}
	switch req.Mode {
	case "", "cascade", "chispa":
	default:
		return nil, statusf(http.StatusBadRequest, "mode must be cascade or chispa (VON alone is measured with kling ai eval -von-alone)")
	}
	in := chispa.Input{Text: req.Text, Fields: req.Fields}
	resp := &ClassifyResponse{Task: req.Task}
	finish := func() (*ClassifyResponse, error) {
		d := time.Since(t0)
		resp.LatencyMS = float64(d.Microseconds()) / 1000
		if endpoint == "decide" {
			resp.Decision = resp.Label
		}
		src := resp.Source
		if resp.Escalate && src == "chispa" {
			src = "escalated" // contestó Chispa sin llegar a su umbral
		}
		g.met.answer(endpoint, req.Task, src, d)
		return resp, nil
	}

	mc := cfg.Models[tc.Chispa]
	casc := g.cascade(req.Task)
	escalar := casc.On() && req.Mode != "chispa"

	// La predicción sale de dos sitios: dentro de este proceso (chispaCache, el
	// camino de siempre) o de una réplica en una microVM (kling chispa deploy,
	// docs/chispa-serverless.md). A partir de aquí el resto de la función no sabe
	// cuál fue: p, tau, confident y labels ya bastan.
	var p chispa.Prediction
	var tau float64
	var confident bool
	var labels []string
	if mc.Backend == BackendMicroVM {
		gp, gl, err := g.classifyGuest(ctx, mc.Snapshot, in, req.Explain)
		if err != nil {
			var we *wakeError
			var dle *deployLookupError
			var gie *guestInvalidError
			reason, code, verb := "request", http.StatusServiceUnavailable, "unavailable"
			switch {
			case errors.As(err, &we):
				reason = "wake"
			case errors.As(err, &dle):
				reason = "labels"
			case errors.As(err, &gie):
				// El invitado SÍ contestó, pero con algo que no es de fiar
				// (chispaguest.go): no es que no haya réplica, es que mintió o se
				// desincronizó con el registro de despliegue. 502, no 503:
				// reintentar no arregla una respuesta que no pasa validación.
				reason, code, verb = "invalid", http.StatusBadGateway, "sent an invalid answer"
			}
			g.met.vonErr("chispa:"+tc.Chispa, reason)
			log.Printf("task %s: chispa %s (microvm): %v", req.Task, tc.Chispa, err)
			return nil, statusf(code, "chispa model %q (backend microvm) %s: %v", tc.Chispa, verb, err)
		}
		p = gp
		tau = p.Threshold
		if v, ok := tc.Thresholds[p.Label]; ok {
			tau = v
		}
		// confident se recalcula aquí, del prob ya validado y el umbral del
		// lado del gateway: la respuesta del invitado puede traer su propio
		// campo "confident", pero no es de fiar (chispaguest.go), así que se
		// ignora y se decide con los mismos datos que un modelo en proceso.
		confident = p.Prob >= tau
		labels = gl
	} else {
		m, err := g.chispa.get(mc.Path)
		if err != nil {
			log.Printf("task %s: loading chispa model: %v", req.Task, err)
			return nil, statusf(http.StatusServiceUnavailable, "chispa model %q unavailable", tc.Chispa)
		}
		p, tau, confident = chispaDecide(m, tc, in, req.Explain)
		labels = m.Labels
	}

	dec := chispa.DecisionEscalate
	if confident {
		dec = chispa.DecisionConfident
	}
	resp.Chispa = &ChispaAnswer{Label: p.Label, Prob: p.Prob, Threshold: tau, Decision: dec}
	resp.Label, resp.Prob, resp.Source, resp.Evidence = p.Label, p.Prob, "chispa", p.Evidence
	if confident {
		if escalar {
			g.maybeAudit(req.Task, tc, cfg, labels, in, p)
		}
		return finish()
	}

	// Chispa duda: la distribución completa y la evidencia van en la respuesta
	// (y en la pregunta a VON, si la plantilla las usa). En proceso hace falta
	// pedirla aparte si no se pidió ya con Explain; una réplica en microvm ya
	// la manda siempre que duda (ver cmd/kling-chispa), así que aquí no hay
	// segunda vuelta que dar.
	resp.Escalate = true
	if mc.Backend != BackendMicroVM && !req.Explain {
		m, err := g.chispa.get(mc.Path)
		if err != nil {
			return nil, statusf(http.StatusServiceUnavailable, "chispa model %q unavailable", tc.Chispa)
		}
		p = m.PredictFull(in, 5)
	}
	resp.Evidence = p.Evidence
	resp.Chispa.Candidates = topN(p.Probs, 3)
	if !escalar {
		return finish()
	}

	allowed, chat := g.escalation(tc, labels, in, p)
	t1 := time.Now()
	vctx, cancel := context.WithTimeout(ctx, g.opts.VONTimeout)
	defer cancel()
	ans, err := g.askVON(vctx, cfg.Models[tc.EscalateTo].Snapshot, chat)
	if err != nil {
		var we *wakeError
		reason := "request"
		if errors.As(err, &we) {
			reason = "wake"
		}
		g.met.vonErr(tc.EscalateTo, reason)
		log.Printf("task %s: von %s: %v", req.Task, tc.EscalateTo, err)
		if tc.OnVONError == "error" {
			return nil, &StatusError{Code: http.StatusServiceUnavailable, Msg: fmt.Sprintf("von model %q did not answer: %v", tc.EscalateTo, err)}
		}
		g.met.inc(g.met.degraded, req.Task)
		resp.Degraded = "von did not answer: " + truncUTF8(err.Error(), 200)
		return finish()
	}
	label := parseLabel(ans, allowed)
	resp.VON = &VONAnswer{Model: tc.EscalateTo, Answer: truncUTF8(ans, maxAnswerShown), LatencyMS: float64(time.Since(t1).Microseconds()) / 1000}
	resp.Label, resp.Source, resp.Prob = label, "von", 0
	if label == Unknown {
		g.met.inc(g.met.unknown, req.Task)
	} else {
		resp.Prob = probOf(p.Probs, label)
		// Muestra para recalibrar: lo escalado entra siempre, con peso 1.
		g.record(req.Task, sample{pred: p.Label, prob: p.Prob, teacher: label, weight: 1, at: time.Now()})
	}
	return finish()
}

// chatFor arma la pregunta a VON de una escalada.
func (g *Gateway) chatFor(tc *TaskConfig, labels []string, in chispa.Input, cands []chispa.ClassProb) chatReq {
	sys := tc.System
	if sys == "" {
		sys = defaultSystem
	}
	mt := tc.MaxTokens
	if mt == 0 {
		mt = 16
	}
	r := chatReq{
		Messages:  []von.Message{{Role: "system", Content: sys}, {Role: "user", Content: renderPrompt(tc.Prompt, labels, in, cands)}},
		MaxTokens: mt,
		Seed:      seed(),
	}
	if tc.Grammar == nil || *tc.Grammar {
		r.Grammar = grammarFor(labels)
	}
	return r
}

// maybeAudit pregunta a VON, en segundo plano, por una fracción de lo que Chispa
// contestó confiado. Solo sirve a la recalibración; el cliente ya tiene su
// respuesta. Una a la vez: si ya hay una en vuelo, se descarta (y se cuenta),
// porque una auditoría no debe provocar réplicas ni colas. Solo con la cascada
// activa (quien llama lo comprueba): con ella apagada, VON no se toca.
func (g *Gateway) maybeAudit(task string, tc *TaskConfig, cfg *Config, labels []string, in chispa.Input, p chispa.Prediction) {
	if tc.Audit <= 0 || tc.EscalateTo == "" || rand.Float64() >= tc.Audit {
		return
	}
	select {
	case g.auditSem <- struct{}{}:
	default:
		g.met.inc(g.met.audits, task, "dropped")
		return
	}
	g.met.inc(g.met.audits, task, "sent")
	// El texto se copia: el de la petición muere con ella.
	in = chispa.Input{Text: string([]byte(in.Text)), Fields: in.Fields}
	snap := cfg.Models[tc.EscalateTo].Snapshot
	req := g.chatFor(tc, labels, in, nil)
	w := 1 / tc.Audit
	go func() {
		defer func() { <-g.auditSem }()
		ctx, cancel := context.WithTimeout(context.Background(), g.opts.VONTimeout)
		defer cancel()
		ans, err := g.askVON(ctx, snap, req)
		if err != nil {
			g.met.vonErr(tc.EscalateTo, "audit")
			return
		}
		if l := parseLabel(ans, labels); l != Unknown {
			g.record(task, sample{pred: p.Label, prob: p.Prob, teacher: l, weight: w, at: time.Now()})
		}
	}()
}

// ---- generación (VON)

// GenerateRequest es el cuerpo de /v1/generate: una tarea de generación y lo
// que va en su plantilla.
type GenerateRequest struct {
	Task  string `json:"task"`
	Input string `json:"input"`
	// Vars son variables extra de la plantilla ({nombre}).
	Vars map[string]string `json:"vars,omitempty"`
	// MaxTokens y Temperature sustituyen a los de la tarea; max_tokens no
	// puede pasar del de la tarea.
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	Seed        *int64   `json:"seed,omitempty"`
}

// GenerateResponse es la respuesta.
type GenerateResponse struct {
	Task         string  `json:"task"`
	Model        string  `json:"model"`
	Output       string  `json:"output"`
	FinishReason string  `json:"finish_reason,omitempty"`
	Usage        Usage   `json:"usage"`
	LatencyMS    float64 `json:"latency_ms"`
}

// Usage son los tokens de una generación.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Límites de las variables de una plantilla: van dentro del prompt, que tiene
// el contexto de la réplica (2048 tokens por defecto).
const (
	maxVars     = 16
	maxVarBytes = 4 << 10
)

// Generate rellena la plantilla de una tarea de generación y pregunta a VON.
func (g *Gateway) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	t0 := time.Now()
	cfg := g.config()
	tc := cfg.Tasks[req.Task]
	if tc == nil {
		return nil, statusf(http.StatusNotFound, "unknown task %q", req.Task)
	}
	if !tc.IsGenerate() {
		return nil, statusf(http.StatusBadRequest, "task %q is a classification task: use POST /v1/classify", req.Task)
	}
	if len(req.Input) > maxText {
		return nil, statusf(http.StatusRequestEntityTooLarge, "input larger than %d bytes", maxText)
	}
	if len(req.Vars) > maxVars {
		return nil, statusf(http.StatusBadRequest, "at most %d vars", maxVars)
	}
	for k, v := range req.Vars {
		if !nameRE.MatchString(k) || k == "input" || len(v) > maxVarBytes {
			return nil, statusf(http.StatusBadRequest, "var %q: names are lowercase letters, digits, . _ - (not \"input\"), values up to %d bytes", k, maxVarBytes)
		}
	}
	mt := tc.MaxTokens
	if mt == 0 {
		mt = 256
	}
	if req.MaxTokens < 0 || req.MaxTokens > mt {
		return nil, statusf(http.StatusBadRequest, "max_tokens must be 0..%d (the task's)", mt)
	}
	if req.MaxTokens > 0 {
		mt = req.MaxTokens
	}
	temp := 0.7
	if tc.Temperature != nil {
		temp = *tc.Temperature
	}
	if req.Temperature != nil {
		if !(*req.Temperature >= 0 && *req.Temperature <= 2) {
			return nil, statusf(http.StatusBadRequest, "temperature must be in [0,2]")
		}
		temp = *req.Temperature
	}
	sd := seed()
	if req.Seed != nil {
		sd = *req.Seed
	}
	var msgs []von.Message
	if tc.System != "" {
		msgs = append(msgs, von.Message{Role: "system", Content: tc.System})
	}
	msgs = append(msgs, von.Message{Role: "user", Content: renderGenerate(tc.Prompt, req.Input, req.Vars)})
	chat := chatReq{Messages: msgs, MaxTokens: mt, Temperature: temp, Seed: sd, JSONSchema: tc.JSONSchema}

	vctx, cancel := context.WithTimeout(ctx, g.opts.ProxyTimeout)
	defer cancel()
	out, err := g.chatVON(vctx, cfg.Models[tc.VON].Snapshot, chat)
	if err != nil {
		var we *wakeError
		if errors.As(err, &we) {
			g.met.vonErr(tc.VON, "wake")
			return nil, &StatusError{Code: http.StatusServiceUnavailable, Msg: err.Error()}
		}
		g.met.vonErr(tc.VON, "request")
		return nil, &StatusError{Code: http.StatusBadGateway, Msg: fmt.Sprintf("von model %q did not answer: %v", tc.VON, err)}
	}
	resp := &GenerateResponse{Task: req.Task, Model: tc.VON, Output: out.Text(),
		Usage: Usage{PromptTokens: out.Usage.PromptTokens, CompletionTokens: out.Usage.CompletionTokens}}
	if len(out.Choices) > 0 {
		resp.FinishReason = out.Choices[0].FinishReason
	}
	// Con esquema, la gramática de llama-server ya obliga a que sea JSON; se
	// comprueba igual porque el invitado no es de fiar, y porque una respuesta
	// cortada por max_tokens es JSON a medias: mejor un error que lo diga que
	// un 200 que el cliente no puede leer.
	if len(tc.JSONSchema) > 0 && !json.Valid([]byte(resp.Output)) {
		g.met.vonErr(tc.VON, "invalid_json")
		return nil, &StatusError{Code: http.StatusBadGateway,
			Msg: fmt.Sprintf("von model %q did not return valid JSON for the task's json_schema (finish_reason %q; raise max_tokens if it is \"length\")", tc.VON, resp.FinishReason)}
	}
	d := time.Since(t0)
	resp.LatencyMS = float64(d.Microseconds()) / 1000
	g.met.answer("generate", req.Task, "von", d)
	return resp, nil
}

func (g *Gateway) record(task string, s sample) {
	g.cfgMu.RLock()
	r := g.rings[task]
	g.cfgMu.RUnlock()
	if r != nil {
		r.add(s)
	}
}

func topN(ps []chispa.ClassProb, n int) []chispa.ClassProb {
	if len(ps) > n {
		ps = ps[:n]
	}
	return ps
}

func probOf(ps []chispa.ClassProb, label string) float64 {
	for _, c := range ps {
		if c.Label == label {
			return c.Prob
		}
	}
	return 0
}

// ---- recalibración

// Calibrate reajusta los umbrales de la tarea con sus muestras. Escribe un .chispa
// nuevo (guardando el anterior en <ruta>.prev) solo si mejora la promesa en la
// mitad de evaluación y no es DryRun.
func (g *Gateway) Calibrate(req CalibrateRequest) (*CalibrateReport, error) {
	g.calMu.Lock()
	defer g.calMu.Unlock()
	cfg := g.config()
	tc := cfg.Tasks[req.Task]
	if tc == nil {
		return nil, statusf(http.StatusNotFound, "unknown task %q", req.Task)
	}
	if tc.Chispa == "" || tc.EscalateTo == "" {
		return nil, statusf(http.StatusBadRequest, "task %q needs a chispa model and escalate_to (the teacher) to recalibrate", req.Task)
	}
	mc := cfg.Models[tc.Chispa]
	if mc.Backend == BackendMicroVM {
		// Recalibrar reescribe el .chispa en disco (más abajo: Marshal, guardar
		// <ruta>.prev, Save) y lo hace con el mismo *chispa.Model que sirve las
		// peticiones (g.chispa.get(path)): un modelo backend microvm no tiene
		// "path" (Validate lo exige vacío, config.go) porque vive horneado
		// DENTRO de la imagen de una réplica, que puede correr en otra máquina
		// o al otro lado de un SSH. No hay fichero local que reescribir ni
		// dorado que journal actualizar in situ, así que en vez de fallar con
		// el "unavailable" genérico de un Path vacío (g.chispa.get("")), se
		// rechaza aquí con lo único que sí funciona hoy: reentrenar y volver a
		// desplegar.
		return nil, statusf(http.StatusBadRequest,
			"task %q: chispa model %q has backend microvm; calibrate needs the model file, and a microvm model has none locally (it lives baked into the replica's image). "+
				"Recalibrate by retraining and redeploying: kling chispa train ... -o new.chispa && kling chispa deploy %s -model new.chispa -replace",
			req.Task, tc.Chispa, tc.Chispa)
	}
	path := mc.Path
	m, err := g.chispa.get(path)
	if err != nil {
		return nil, statusf(http.StatusServiceUnavailable, "chispa model %q unavailable: %v", tc.Chispa, err)
	}
	target := req.Target
	if target == 0 {
		target = tc.Precision
	}
	if target == 0 {
		target = m.Meta.TargetPrecision
	}
	if target == 0 {
		target = 0.95
	}
	if !(target > 0 && target < 1) {
		return nil, statusf(http.StatusBadRequest, "target must be in (0,1)")
	}
	ms := req.MinSupport
	if ms <= 0 {
		ms = 10
	}
	g.cfgMu.RLock()
	r := g.rings[req.Task]
	g.cfgMu.RUnlock()
	rep, nuevo := calibrate(m, r.snapshot(), tc.Thresholds, target, ms)
	rep.Task, rep.Model = req.Task, tc.Chispa
	if !rep.Improved || req.DryRun {
		return rep, nil
	}

	// El modelo se copia pasando por su formato (no hay otra copia segura: el
	// struct lleva un sync.Pool) y solo cambian los umbrales y una nota.
	b, err := m.Marshal()
	if err != nil {
		return nil, err
	}
	nm, err := chispa.Unmarshal(b)
	if err != nil {
		return nil, err
	}
	nm.Thresholds = nuevo
	nm.Meta.Notes = fmt.Sprintf("%s[recalibrated %s by kling ai calibrate: %d samples, VON %q as teacher, target agreement %.2f] ",
		nm.Meta.Notes, time.Now().UTC().Format(time.RFC3339), rep.Samples, tc.EscalateTo, target)
	nb, err := nm.Marshal()
	if err != nil {
		return nil, err
	}
	if nm, err = chispa.Unmarshal(nb); err != nil { // lo que se sirve es lo que se leería del disco
		return nil, err
	}
	backup := path + ".prev"
	if err := os.WriteFile(backup, b, 0o644); err != nil {
		return nil, fmt.Errorf("saving backup: %w", err)
	}
	if err := nm.Save(path); err != nil {
		return nil, err
	}
	g.chispa.put(path, nm)
	rep.Written, rep.Backup = path, backup
	log.Printf("task %s: recalibrated %s (%s)", req.Task, path, rep.Reason)
	// Umbrales nuevos = otro reparto entre lo que contesta Chispa y lo que
	// escala: la evaluación que respaldaba la cascada ya no describe lo que se
	// sirve. Se vuelve a decidir (con escalate_force sigue activa).
	before := g.cascade(req.Task)
	g.regate()
	if after := g.cascade(req.Task); before.On() && !after.On() {
		rep.Cascade = "the cascade is off until it is evaluated again: " + after.Reason
	}
	return rep, nil
}
