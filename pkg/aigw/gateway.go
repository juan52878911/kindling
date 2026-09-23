package aigw

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/jev"
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

	ID             string        // valor de LabelGateway; "default"
	NamePrefix     string        // prefijo de las máquinas; "gw-"
	Idle           time.Duration // sin peticiones antes de congelar una réplica; 2 min
	MaxReplicas    int           // por modelo, salvo max_replicas en el registro; 2
	MaxInflight    int           // peticiones por réplica antes de pedir otra; 1
	KeepWarm       int           // N modelos populares siempre despiertos; 0
	JEVBudget      int64         // bytes de modelos JEV cargados; 256 MiB
	VONTimeout     time.Duration // plazo de una escalada; 60 s
	ProxyTimeout   time.Duration // plazo de una petición OpenAI; 5 min
	MaxBody        int64         // cuerpo de una petición; 1 MiB
	MaxProxyBytes  int64         // respuesta de una réplica por el proxy; 32 MiB
	PopularityFile string        // "" = solo en memoria
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
	if o.JEVBudget == 0 {
		o.JEVBudget = 256 << 20
	}
	if o.VONTimeout <= 0 {
		o.VONTimeout = 60 * time.Second
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
}

// Gateway es el gateway de IA.
type Gateway struct {
	opts     Options
	sched    *scheduler.Scheduler
	replicas Replicas
	jev      *jevCache
	met      *metrics

	cfgMu sync.RWMutex
	cfg   *Config
	rings map[string]*ring // tarea -> muestras; bajo cfgMu

	auditSem chan struct{} // auditorías en vuelo (una): nunca compiten en masa con el tráfico
	calMu    sync.Mutex    // una recalibración a la vez

	repMu    sync.Mutex // caché de réplicas por modelo para /metrics
	repAt    time.Time
	repCache map[string]replicaCount
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
	g := &Gateway{opts: o, jev: newJEVCache(o.JEVBudget), met: newMetrics(), auditSem: make(chan struct{}, 1)}
	g.setConfig(cfg)

	s := scheduler.New(o.Client, o.Idle, false, 0)
	s.Port = von.Port
	s.MachineLabels = map[string]string{LabelGateway: o.ID}
	s.NamePrefix = o.NamePrefix
	// Sin TTL de red de seguridad: el daemon lo cuenta desde la creación y un
	// thaw no lo reinicia, así que una réplica vieja se re-congelaría bajo
	// tráfico. A cambio, Start congela lo que un gateway anterior dejó
	// corriendo, y Close congela lo suyo al salir.
	s.MachineTTL = -1
	s.MaxInflight = o.MaxInflight
	s.MaxReplicas = o.MaxReplicas
	s.KeepWarm = o.KeepWarm
	s.SetPopularityFile(o.PopularityFile)
	s.MaxReplicasFor = func(snap string) int {
		if _, m := g.config().vonModel(snap); m != nil {
			return m.MaxReplicas
		}
		return 0
	}
	// El keepwarm recorre TODOS los snapshots del daemon: solo los dorados
	// registrados como modelos VON son de este gateway.
	s.Skip = func(sn *api.Snapshot) bool {
		_, m := g.config().vonModel(sn.Name)
		return m == nil
	}
	s.OnAcquire = func(snap, how string, d time.Duration) {
		name, _ := g.config().vonModel(snap)
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

// setConfig instala un registro. Las muestras de las tareas que siguen
// existiendo se conservan (se copian si cambió el tamaño del anillo).
func (g *Gateway) setConfig(c *Config) {
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
}

// Reload relee el registro del disco. Los modelos JEV se vuelven a leer en su
// siguiente uso (el fichero pudo cambiar).
func (g *Gateway) Reload() error {
	if g.opts.ConfigPath == "" {
		return errors.New("no config file to reload")
	}
	c, err := LoadConfig(g.opts.ConfigPath)
	if err != nil {
		return err
	}
	old := g.config()
	for _, m := range old.Models {
		if m.Kind == KindJEV {
			g.jev.drop(m.Path)
		}
	}
	g.setConfig(c)
	return nil
}

// Start pone en marcha el segador (congela réplicas ociosas, mantiene el
// keepwarm) y congela lo que un gateway anterior con el mismo id dejara
// corriendo sin nadie que lo vigile. Vuelve enseguida; el segador para con ctx.
func (g *Gateway) Start(ctx context.Context) {
	if g.opts.Client == nil {
		return
	}
	if n := g.freezeOwn(ctx); n > 0 {
		log.Printf("froze %d replica(s) left running by a previous gateway %q", n, g.opts.ID)
	}
	go g.sched.Reap(ctx)
}

// Close congela las réplicas de este gateway que sigan corriendo: al salir no
// queda nada gastando CPU ni RAM, y la próxima vez vuelven con un thaw.
func (g *Gateway) Close(ctx context.Context) {
	if g.opts.Client == nil {
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

// ---- la cascada

// ClassifyRequest es el cuerpo de /v1/classify y /v1/decide.
type ClassifyRequest struct {
	Task   string         `json:"task"`
	Text   string         `json:"text"`
	Fields map[string]any `json:"fields,omitempty"`
	// Mode: "cascade" (por defecto), "jev" (solo JEV, aunque dude) o "von"
	// (siempre VON). Los dos últimos son para medir y comparar.
	Mode string `json:"mode,omitempty"`
	// Explain añade la evidencia también a las respuestas confiadas de JEV
	// (cuesta reservas de memoria; en las escaladas va siempre).
	Explain bool `json:"explain,omitempty"`
}

// ClassifyResponse es la respuesta.
type ClassifyResponse struct {
	Task      string         `json:"task"`
	Label     string         `json:"label"`
	Decision  string         `json:"decision,omitempty"` // /v1/decide: la misma etiqueta
	Prob      float64        `json:"prob"`               // probabilidad calibrada de JEV para Label (0 si no hay JEV o es unknown)
	Source    string         `json:"source"`             // jev | von
	LatencyMS float64        `json:"latency_ms"`
	Evidence  []jev.Evidence `json:"evidence,omitempty"`
	JEV       *JEVAnswer     `json:"jev,omitempty"`
	VON       *VONAnswer     `json:"von,omitempty"`
	Degraded  string         `json:"degraded,omitempty"` // por qué contestó JEV sin llegar a su umbral
}

// JEVAnswer es lo que dijo JEV, conteste él o no.
type JEVAnswer struct {
	Label      string          `json:"label"`
	Prob       float64         `json:"prob"`
	Threshold  float64         `json:"threshold"`
	Decision   string          `json:"decision"`
	Candidates []jev.ClassProb `json:"candidates,omitempty"`
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

// maxText acota el texto a clasificar. JEV lo recorta a su max_text de todas
// formas; esto evita guardar y copiar megas por petición.
const maxText = 64 << 10

// Classify es la cascada JEV → VON.
func (g *Gateway) Classify(ctx context.Context, endpoint string, req ClassifyRequest) (*ClassifyResponse, error) {
	t0 := time.Now()
	cfg := g.config()
	tc := cfg.Tasks[req.Task]
	if tc == nil {
		return nil, statusf(http.StatusNotFound, "unknown task %q", req.Task)
	}
	if len(req.Text) > maxText {
		return nil, statusf(http.StatusRequestEntityTooLarge, "text larger than %d bytes", maxText)
	}
	switch req.Mode {
	case "", "cascade", "jev", "von":
	default:
		return nil, statusf(http.StatusBadRequest, "mode must be cascade, jev or von")
	}
	if req.Mode == "jev" && tc.JEV == "" {
		return nil, statusf(http.StatusBadRequest, "task %q has no jev model", req.Task)
	}
	if req.Mode == "von" && tc.VON == "" {
		return nil, statusf(http.StatusBadRequest, "task %q has no von model", req.Task)
	}
	in := jev.Input{Text: req.Text, Fields: req.Fields}
	resp := &ClassifyResponse{Task: req.Task}
	finish := func() (*ClassifyResponse, error) {
		d := time.Since(t0)
		resp.LatencyMS = float64(d.Microseconds()) / 1000
		if endpoint == "decide" {
			resp.Decision = resp.Label
		}
		g.met.answer(endpoint, req.Task, resp.Source, d)
		return resp, nil
	}

	labels := tc.Labels
	var m *jev.Model
	var p jev.Prediction
	if tc.JEV != "" {
		var err error
		if m, err = g.jev.get(cfg.Models[tc.JEV].Path); err != nil {
			log.Printf("task %s: loading jev model: %v", req.Task, err)
			return nil, statusf(http.StatusServiceUnavailable, "jev model %q unavailable", tc.JEV)
		}
		labels = m.Labels
		if req.Mode != "von" {
			if req.Explain {
				p = m.PredictFull(in, 5)
			} else {
				p = m.Predict(in)
			}
			tau := p.Threshold
			if v, ok := tc.Thresholds[p.Label]; ok {
				tau = v
			}
			confident := p.Prob >= tau
			dec := jev.DecisionEscalate
			if confident {
				dec = jev.DecisionConfident
			}
			resp.JEV = &JEVAnswer{Label: p.Label, Prob: p.Prob, Threshold: tau, Decision: dec}
			if confident || req.Mode == "jev" || tc.VON == "" {
				resp.Label, resp.Prob, resp.Source, resp.Evidence = p.Label, p.Prob, "jev", p.Evidence
				if !confident && req.Mode != "jev" {
					resp.Degraded = "no von model for this task: jev answered below its threshold"
				}
				if confident && req.Mode != "jev" {
					g.maybeAudit(req.Task, tc, cfg, labels, in, p)
				}
				return finish()
			}
		}
		// Escala: la distribución completa y la evidencia van en la respuesta
		// y, si la plantilla las usa, en la pregunta.
		full := m.PredictFull(in, 5)
		p = full
		resp.Evidence = full.Evidence
		if resp.JEV == nil {
			resp.JEV = &JEVAnswer{Label: full.Label, Prob: full.Prob, Threshold: full.Threshold, Decision: full.Decision}
		}
		resp.JEV.Candidates = topN(full.Probs, 3)
	}

	t1 := time.Now()
	vctx, cancel := context.WithTimeout(ctx, g.opts.VONTimeout)
	defer cancel()
	ans, err := g.askVON(vctx, cfg.Models[tc.VON].Snapshot, g.chatFor(tc, labels, in, p.Probs))
	if err != nil {
		var we *wakeError
		reason := "request"
		if errors.As(err, &we) {
			reason = "wake"
		}
		g.met.vonErr(tc.VON, reason)
		log.Printf("task %s: von %s: %v", req.Task, tc.VON, err)
		if m == nil || tc.OnVONError == "error" || req.Mode == "von" {
			return nil, &StatusError{Code: http.StatusServiceUnavailable, Msg: fmt.Sprintf("von model %q did not answer: %v", tc.VON, err)}
		}
		g.met.inc(g.met.degraded, req.Task)
		resp.Label, resp.Prob, resp.Source = p.Label, p.Prob, "jev"
		resp.Degraded = "von did not answer: " + truncUTF8(err.Error(), 200)
		return finish()
	}
	label := parseLabel(ans, labels)
	resp.VON = &VONAnswer{Model: tc.VON, Answer: truncUTF8(ans, maxAnswerShown), LatencyMS: float64(time.Since(t1).Microseconds()) / 1000}
	resp.Label, resp.Source = label, "von"
	if label == Unknown {
		g.met.inc(g.met.unknown, req.Task)
	} else if m != nil {
		resp.Prob = probOf(p.Probs, label)
		// Muestra para recalibrar: lo escalado entra siempre (peso 1), y con
		// mode=von también todo lo demás (peso 1: no hay selección).
		g.record(req.Task, sample{pred: p.Label, prob: p.Prob, teacher: label, weight: 1, at: time.Now()})
	}
	return finish()
}

// chatFor arma la pregunta a VON de una tarea.
func (g *Gateway) chatFor(tc *TaskConfig, labels []string, in jev.Input, cands []jev.ClassProb) chatReq {
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

// maybeAudit pregunta a VON, en segundo plano, por una fracción de lo que JEV
// contestó confiado. Solo sirve a la recalibración; el cliente ya tiene su
// respuesta. Una a la vez: si ya hay una en vuelo, se descarta (y se cuenta),
// porque una auditoría no debe provocar réplicas ni colas.
func (g *Gateway) maybeAudit(task string, tc *TaskConfig, cfg *Config, labels []string, in jev.Input, p jev.Prediction) {
	if tc.Audit <= 0 || tc.VON == "" || rand.Float64() >= tc.Audit {
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
	in = jev.Input{Text: string([]byte(in.Text)), Fields: in.Fields}
	snap := cfg.Models[tc.VON].Snapshot
	req := g.chatFor(tc, labels, in, nil)
	w := 1 / tc.Audit
	go func() {
		defer func() { <-g.auditSem }()
		ctx, cancel := context.WithTimeout(context.Background(), g.opts.VONTimeout)
		defer cancel()
		ans, err := g.askVON(ctx, snap, req)
		if err != nil {
			g.met.vonErr(tc.VON, "audit")
			return
		}
		if l := parseLabel(ans, labels); l != Unknown {
			g.record(task, sample{pred: p.Label, prob: p.Prob, teacher: l, weight: w, at: time.Now()})
		}
	}()
}

func (g *Gateway) record(task string, s sample) {
	g.cfgMu.RLock()
	r := g.rings[task]
	g.cfgMu.RUnlock()
	if r != nil {
		r.add(s)
	}
}

func topN(ps []jev.ClassProb, n int) []jev.ClassProb {
	if len(ps) > n {
		ps = ps[:n]
	}
	return ps
}

func probOf(ps []jev.ClassProb, label string) float64 {
	for _, c := range ps {
		if c.Label == label {
			return c.Prob
		}
	}
	return 0
}

// ---- recalibración

// Calibrate reajusta los umbrales de la tarea con sus muestras. Escribe un .jev
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
	if tc.JEV == "" || tc.VON == "" {
		return nil, statusf(http.StatusBadRequest, "task %q needs both a jev and a von model to recalibrate", req.Task)
	}
	path := cfg.Models[tc.JEV].Path
	m, err := g.jev.get(path)
	if err != nil {
		return nil, statusf(http.StatusServiceUnavailable, "jev model %q unavailable: %v", tc.JEV, err)
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
	rep.Task, rep.Model = req.Task, tc.JEV
	if !rep.Improved || req.DryRun {
		return rep, nil
	}

	// El modelo se copia pasando por su formato (no hay otra copia segura: el
	// struct lleva un sync.Pool) y solo cambian los umbrales y una nota.
	b, err := m.Marshal()
	if err != nil {
		return nil, err
	}
	nm, err := jev.Unmarshal(b)
	if err != nil {
		return nil, err
	}
	nm.Thresholds = nuevo
	nm.Meta.Notes = fmt.Sprintf("%s[recalibrated %s by kling ai calibrate: %d samples, VON %q as teacher, target agreement %.2f] ",
		nm.Meta.Notes, time.Now().UTC().Format(time.RFC3339), rep.Samples, tc.VON, target)
	nb, err := nm.Marshal()
	if err != nil {
		return nil, err
	}
	if nm, err = jev.Unmarshal(nb); err != nil { // lo que se sirve es lo que se leería del disco
		return nil, err
	}
	backup := path + ".prev"
	if err := os.WriteFile(backup, b, 0o644); err != nil {
		return nil, fmt.Errorf("saving backup: %w", err)
	}
	if err := nm.Save(path); err != nil {
		return nil, err
	}
	g.jev.put(path, nm)
	rep.Written, rep.Backup = path, backup
	log.Printf("task %s: recalibrated %s (%s)", req.Task, path, rep.Reason)
	return rep, nil
}
