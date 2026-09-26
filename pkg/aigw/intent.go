package aigw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/chispa"
	"github.com/juan52878911/kindling/pkg/chispa/slots"
	"github.com/juan52878911/kindling/pkg/codificador"
	"github.com/juan52878911/kindling/pkg/intent"
	"github.com/juan52878911/kindling/pkg/scheduler"
)

// TAREAS DE INTENCIÓN: /v1/decide para órdenes con intención y huecos.
//
// La decisión es la cascada de pkg/intent: las plantillas del dominio
// (capa 1), Chispa + Chispa-slots (capa 2: en proceso, microsegundos; o
// serverless, una réplica de `kling chispa deploy` que se despierta con la
// orden si el modelo de intención es backend "microvm", ver guestIntent) y,
// para lo que esas dudan, el codificador de frases (capa 3): una réplica de
// un dorado kind embed que pkg/scheduler despierta con la primera petición y
// congela al quedarse ociosa, igual que un VON, y la cabeza .jenc que
// clasifica su vector aquí mismo. Lo que tampoco resuelve sale con escalate:
// "von".
//
// Lo que sabe del dominio (plantillas, cómo se normaliza un hueco, qué
// necesita cada intención) lo pone un intent.Domain: uno de datos
// ("schema": un fichero JSON, intent.Schema) o uno en Go que el programa que
// embebe el gateway registra en Options.Domains ("domain": su nombre).
//
// La capa 3 se enciende como la cascada Chispa → VON: solo si un registro de
// evaluación (`kling ai eval <tarea> -data rows.jsonl`) muestra que la
// cascada con el codificador acierta la orden COMPLETA (intención y huecos)
// más veces que sin él (McNemar, p < 0,05), con los mismos modelos que se
// sirven. Si no, la tarea contesta con las capas rápidas y escala a
// "encoder". Ver docs/intent.md.

// IntentConfig es una tarea de intención.
type IntentConfig struct {
	// Model es el modelo Chispa de intención (kind chispa).
	Model string `json:"model"`
	// Domain es el nombre de un dominio en Go registrado en
	// Options.Domains; Schema, la ruta de un intent.Schema (relativa al
	// registro). Uno de los dos.
	Domain string `json:"domain,omitempty"`
	Schema string `json:"schema,omitempty"`
	// Slots es el etiquetador de huecos (.chispas); relativo al registro.
	Slots string `json:"slots,omitempty"`
	// Encoder es el codificador (kind embed) y Head la cabeza entrenada sobre
	// SUS vectores (.jenc, relativa al registro). Los dos o ninguno.
	Encoder string `json:"encoder,omitempty"`
	Head    string `json:"head,omitempty"`
	// EncoderForce enciende la capa 3 sin una evaluación que la respalde.
	EncoderForce bool `json:"encoder_force,omitempty"`
	// FinalOOS da por buena una respuesta «fuera de ámbito» confiada (por
	// defecto escala: ahí caen las órdenes indirectas).
	FinalOOS bool `json:"final_oos,omitempty"`
}

func (c *Config) validateIntent(n string, t *TaskConfig) []error {
	var errs []error
	d := t.Intent
	if t.Chispa != "" || t.VON != "" || t.EscalateTo != "" || t.EscalateForce || len(t.Labels) > 0 || t.System != "" ||
		t.Prompt != "" || t.TopK != 0 || len(t.Thresholds) > 0 || t.Precision != 0 || t.Audit != 0 || t.Samples != 0 ||
		t.MaxTokens != 0 || t.Temperature != nil || t.Grammar != nil || t.OnVONError != "" {
		errs = append(errs, fmt.Errorf("task %q: an intent task takes only its \"intent\" block", n))
	}
	if m := c.Models[d.Model]; m == nil || m.Kind != KindChispa {
		errs = append(errs, fmt.Errorf("task %q: intent.model %q is not a chispa model", n, d.Model))
	}
	if (d.Domain == "") == (d.Schema == "") {
		errs = append(errs, fmt.Errorf("task %q: intent needs a \"schema\" file or a \"domain\" built into the gateway (one of them)", n))
	}
	if (d.Encoder == "") != (d.Head == "") {
		errs = append(errs, fmt.Errorf("task %q: intent.encoder and intent.head go together", n))
	}
	if d.Encoder != "" {
		if m := c.Models[d.Encoder]; m == nil || m.Kind != KindEmbed {
			errs = append(errs, fmt.Errorf("task %q: intent.encoder %q is not an embed model", n, d.Encoder))
		}
	}
	if d.EncoderForce && d.Encoder == "" {
		errs = append(errs, fmt.Errorf("task %q: encoder_force without an encoder", n))
	}
	return errs
}

// ---- lo que usan las tareas de intención

// intentFiles guarda los .chispas, .jenc y esquemas cargados (pocos y
// pequeños: sin presupuesto). Reload los olvida.
type intentFiles struct {
	mu      sync.Mutex
	slots   map[string]*slots.Model
	heads   map[string]*codificador.Head
	schemas map[string]*intent.Schema
}

func (f *intentFiles) reset() {
	f.mu.Lock()
	f.slots, f.heads, f.schemas = nil, nil, nil
	f.mu.Unlock()
}

// loadSlotsFn, loadHeadFn y loadSchemaFn se sustituyen en los tests.
var (
	loadSlotsFn  = slots.LoadFile
	loadHeadFn   = codificador.LoadFile
	loadSchemaFn = intent.LoadSchema
)

// cached lee el fichero FUERA del candado, como pkg/aigw/chispacache.go: una
// tarea cuyo fichero tarda en leerse (o cuyo disco anda lento) no para las
// decisiones de las demás tareas, que solo tocan el candado para un mapa ya
// en memoria. Dos peticiones que piden a la vez el mismo fichero pueden
// leerlo dos veces (son pocos y pequeños: no compensa la coordinación de
// chispacache), pero la segunda comprobación bajo el candado asegura que solo
// una entrada gana y todo el mundo ve la misma.
func cached[T any](mu *sync.Mutex, m *map[string]*T, p string, load func(string) (*T, error)) (*T, error) {
	mu.Lock()
	v := (*m)[p]
	mu.Unlock()
	if v != nil {
		return v, nil
	}
	v, err := load(p)
	if err != nil {
		return nil, err
	}
	mu.Lock()
	defer mu.Unlock()
	if existing := (*m)[p]; existing != nil {
		return existing, nil
	}
	if *m == nil {
		*m = map[string]*T{}
	}
	(*m)[p] = v
	return v, nil
}

func (f *intentFiles) getSlots(p string) (*slots.Model, error) {
	return cached(&f.mu, &f.slots, p, loadSlotsFn)
}

func (f *intentFiles) getHead(p string) (*codificador.Head, error) {
	return cached(&f.mu, &f.heads, p, loadHeadFn)
}

func (f *intentFiles) getSchema(p string) (*intent.Schema, error) {
	return cached(&f.mu, &f.schemas, p, loadSchemaFn)
}

// domain es el intent.Domain de una tarea.
func (g *Gateway) domain(d *IntentConfig) (intent.Domain, error) {
	if d.Domain != "" {
		dom := g.opts.Domains[d.Domain]
		if dom == nil {
			return nil, fmt.Errorf("domain %q is not built into this gateway (a Go domain comes from the program that embeds pkg/aigw; use \"schema\" for a data file)", d.Domain)
		}
		return dom, nil
	}
	s, err := g.files.getSchema(d.Schema)
	if err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return s, nil
}

// replicaEmbedder pide vectores a una réplica del dorado del codificador,
// despertándola si hace falta (el mismo camino que una escalada a VON).
type replicaEmbedder struct {
	g    *Gateway
	snap string
	dim  int
}

func (e replicaEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := codificador.EmbedBody(texts)
	if err != nil {
		return nil, err
	}
	resp, rep, err := e.g.postGuest(ctx, e.snap, "/v1/embeddings", body)
	if err != nil {
		return nil, err
	}
	defer rep.Release()
	defer resp.Body.Close()
	return codificador.ParseEmbeddings(resp.StatusCode, resp.Body, len(texts), e.dim)
}

// guestIntent es la capa 2 servida serverless: el mismo camino que una tarea
// de clasificación con backend microvm (askChispaGuest: etiquetas y huecos
// validados contra el registro de despliegue, confianza del lado del
// gateway), más cómo estaba la microVM para la traza.
type guestIntent struct {
	g     *Gateway
	model string
	snap  string
}

func (r guestIntent) ClassifyIntent(ctx context.Context, text, lang string) (intent.RemoteAnswer, error) {
	a, err := r.g.askChispaGuest(ctx, r.snap, chispa.Input{Text: text, Fields: intent.LangFields(lang)}, false)
	out := intent.RemoteAnswer{Replica: replicaInfo(r.model, a.Wake, a.Request)}
	if err != nil {
		reason, _, _ := guestErrReason(err)
		r.g.met.vonErr("chispa:"+r.model, reason)
		if a.Wake == nil && a.Request == 0 {
			out.Replica = nil // no llegó a haber réplica
		}
		return out, err
	}
	_, out.Confident = guestConfident(a.Pred, nil)
	out.Label, out.Prob, out.HasSlots, out.Spans = a.Pred.Label, a.Pred.Prob, a.HasSlots, a.Spans
	return out, nil
}

// replicaInfo traduce el despertar del planificador al estado de la traza:
// thaw = estaba congelada, resume = pausada, restore = no había (del dorado),
// y sin despertar (o adopt, ya corría) = despierta.
func replicaInfo(model string, w *scheduler.WakeTrace, req time.Duration) *intent.ReplicaInfo {
	ri := &intent.ReplicaInfo{Model: model, State: intent.ReplicaWarm, RequestMS: msOf(req)}
	if w == nil {
		return ri
	}
	switch w.How {
	case "thaw":
		ri.State = intent.ReplicaFrozen
	case "resume":
		ri.State = intent.ReplicaPaused
	case "restore":
		ri.State = intent.ReplicaNew
	}
	ri.WakeMS = msOf(w.Total)
	return ri
}

func msOf(d time.Duration) float64 { return math.Round(float64(d.Microseconds())/10) / 100 }

// decider arma la cascada de una tarea. withEncoder añade la capa 3 (si la
// tarea la tiene), esté o no encendida: la evaluación la necesita apagada y
// encendida.
func (g *Gateway) decider(cfg *Config, d *IntentConfig, withEncoder bool) (*intent.Decider, error) {
	dom, err := g.domain(d)
	if err != nil {
		return nil, err
	}
	dec := &intent.Decider{Domain: dom, FinalOOS: d.FinalOOS}
	if mc := cfg.Models[d.Model]; mc.Backend == BackendMicroVM {
		dec.Remote = guestIntent{g: g, model: d.Model, snap: mc.Snapshot}
	} else if dec.Intent, err = g.chispa.get(mc.Path); err != nil {
		return nil, fmt.Errorf("intent model: %w", err)
	}
	if d.Slots != "" {
		if dec.Slots, err = g.files.getSlots(d.Slots); err != nil {
			return nil, fmt.Errorf("slot model: %w", err)
		}
	}
	if withEncoder && d.Encoder != "" {
		h, err := g.files.getHead(d.Head)
		if err != nil {
			return nil, fmt.Errorf("encoder head: %w", err)
		}
		dec.Encoder = &codificador.Layer{Head: h, Timeout: g.opts.EncoderTimeout,
			Embedder: replicaEmbedder{g: g, snap: cfg.Models[d.Encoder].Snapshot, dim: h.Dim}}
	}
	return dec, nil
}

// DecideResponse es la respuesta de /v1/decide en una tarea de intención: la
// decisión de pkg/intent, con la intención también como "decision" (lo que
// devuelve /v1/decide en las tareas de clasificación).
type DecideResponse struct {
	// ID identifica la respuesta para /v1/feedback; solo en tareas con "learn".
	ID    string `json:"id,omitempty"`
	Task  string `json:"task"`
	Label string `json:"decision"`
	intent.Decision
	LatencyMS float64 `json:"latency_ms"`
	// Encoder dice si la capa 3 estaba encendida y, si no, por qué (el estado
	// de su evaluación): quien llama no tiene que adivinarlo.
	Encoder *CascadeState `json:"encoder,omitempty"`
}

// validLang: "", "auto" o un código corto de letras (es, en, pt-br…).
func validLang(l string) bool {
	if len(l) > 8 {
		return false
	}
	for _, c := range l {
		if !(c >= 'a' && c <= 'z' || c == '-') {
			return false
		}
	}
	return true
}

// Decide decide una orden de una tarea de intención.
func (g *Gateway) Decide(ctx context.Context, req ClassifyRequest) (*DecideResponse, error) {
	t0 := time.Now()
	cfg := g.config()
	tc := cfg.Tasks[req.Task]
	if tc == nil {
		return nil, statusf(http.StatusNotFound, "unknown task %q", req.Task)
	}
	if tc.Intent == nil {
		return nil, statusf(http.StatusBadRequest, "task %q is not an intent task", req.Task)
	}
	if len(req.Text) > maxText {
		return nil, statusf(http.StatusRequestEntityTooLarge, "text larger than %d bytes", maxText)
	}
	if !validLang(req.Lang) {
		return nil, statusf(http.StatusBadRequest, "lang must be auto or a short language code (es, en…)")
	}
	casc := g.cascade(req.Task)
	dec, err := g.decider(cfg, tc.Intent, casc.On())
	if err != nil {
		log.Printf("task %s: %v", req.Task, err)
		return nil, statusf(http.StatusServiceUnavailable, "task %q: %v", req.Task, err)
	}
	d := dec.DecideContext(ctx, req.Text, req.Lang)
	if d.ChispaError != "" {
		g.met.inc(g.met.degraded, req.Task)
		log.Printf("task %s: chispa %s (microvm): %s", req.Task, tc.Intent.Model, d.ChispaError)
	}
	if d.EncoderError != "" {
		g.met.vonErr(tc.Intent.Encoder, "encoder")
		g.met.inc(g.met.degraded, req.Task)
		log.Printf("task %s: encoder %s: %s", req.Task, tc.Intent.Encoder, d.EncoderError)
	}
	resp := &DecideResponse{Task: req.Task, Label: d.Intent, Decision: d}
	if tc.Intent.Encoder != "" {
		resp.Encoder = &casc
	}
	if ls, ok := g.learnFor(req.Task); ok {
		resp.ID = g.learn.newID()
		fast := d.Confident && (d.Layer == intent.LayerTemplate || d.Layer == intent.LayerChispa)
		g.met.learnAnswer(req.Task, ls.version, fast, ls.cfg.WindowMinutes)
		if !fast && d.Layer != intent.LayerTemplate && ls.cfg.capturing() {
			g.captureDecide(ctx, ls, cfg, tc, req.Task, resp.ID, req.Text, d, dec.Domain.OutOfScope())
		}
	}
	el := time.Since(t0)
	resp.LatencyMS = float64(el.Microseconds()) / 1000
	src := d.Layer
	if !d.Confident {
		src = "escalated"
	}
	g.met.answer("decide", req.Task, src, el)
	return resp, nil
}

// ---- la puerta de la capa 3

// IntentEvalResults son las cifras de la evaluación de una tarea de
// intención: la cascada sin la capa 3 (plantillas → Chispa) y con ella.
type IntentEvalResults struct {
	Examples int `json:"examples"`
	// Orden completa bien (intención y huecos), cubierta confiada y acierto
	// de lo confiado, sin y con el codificador.
	FastExact             float64 `json:"fast_exact"`
	CascadeExact          float64 `json:"cascade_exact"`
	FastCoverage          float64 `json:"fast_coverage"`
	CascadeCoverage       float64 `json:"cascade_coverage"`
	FastPrecision         float64 `json:"fast_precision"`
	CascadePrecision      float64 `json:"cascade_precision"`
	FastConfidentWrong    int     `json:"fast_confident_wrong"`
	CascadeConfidentWrong int     `json:"cascade_confident_wrong"`
	// Lo que llegó al codificador y lo que contestó confiado.
	ToEncoder        int `json:"to_encoder"`
	EncoderConfident int `json:"encoder_confident"`
	EncoderErrors    int `json:"encoder_errors"`
	// Lo que decide la puerta: órdenes CONTESTADAS bien (confiadas y
	// completas). Una capa intermedia no gana por conjeturar mejor lo que
	// escala (eso lo decide VON), sino por contestar bien lo que antes
	// escalaba sin equivocarse más con confianza. Discrepancias y McNemar
	// exacta de una cola sobre eso.
	FastAnswered     float64 `json:"fast_answered_right"`
	CascadeAnswered  float64 `json:"cascade_answered_right"`
	FastOnlyRight    int     `json:"fast_only_right"`
	CascadeOnlyRight int     `json:"cascade_only_right"`
	PValue           float64 `json:"p_value"`
	// Lo mismo contando también la conjetura de lo que escala (el «exact»):
	// informativo.
	ExactFastOnlyRight    int     `json:"exact_fast_only_right"`
	ExactCascadeOnlyRight int     `json:"exact_cascade_only_right"`
	ExactPValue           float64 `json:"exact_p_value"`
	EncoderP50MS          float64 `json:"encoder_p50_ms"`
	EncoderP95MS          float64 `json:"encoder_p95_ms"`
	DurationS             float64 `json:"duration_s"`
}

// IntentEvalRecord es lo que se guarda (ai-evals/<tarea>.json) y lo que
// mira la puerta: la identidad de todo lo que cambia el resultado.
type IntentEvalRecord struct {
	Task string    `json:"task"`
	Kind string    `json:"kind"` // "intent"
	At   time.Time `json:"at"`
	Data struct {
		Name     string `json:"name,omitempty"`
		SHA256   string `json:"sha256"`
		Examples int    `json:"examples"`
	} `json:"data"`
	// Domain es el dominio: "domain:<nombre>" (Go) o "schema:<sha256>".
	Domain string `json:"domain"`
	Intent struct {
		Model  string `json:"model"`
		SHA256 string `json:"sha256"`
	} `json:"intent"`
	SlotsSHA256 string `json:"slots_sha256,omitempty"`
	HeadSHA256  string `json:"head_sha256"`
	Encoder     struct {
		Model    string `json:"model"`
		Snapshot string `json:"snapshot"`
	} `json:"encoder"`
	FinalOOS  bool              `json:"final_oos"`
	Results   IntentEvalResults `json:"results"`
	BeatsFast bool              `json:"beats_fast"`
	Verdict   string            `json:"verdict"`
	Stored    string            `json:"stored,omitempty"`
}

// intentIdentity son los hashes de lo que sirve una tarea de intención.
func intentIdentity(cfg *Config, d *IntentConfig) (domain, model, slotsSum, head string, err error) {
	if d.Domain != "" {
		domain = "domain:" + d.Domain
	} else {
		var s string
		if s, err = fileSHA256(d.Schema); err != nil {
			return
		}
		domain = "schema:" + s
	}
	im := cfg.Models[d.Model]
	if im.Backend == BackendMicroVM {
		// Sin .chispa local que hashear (el modelo vive horneado en el dorado):
		// el nombre del dorado es su identidad, igual que ya hace el codificador
		// (Encoder.Snapshot) más abajo.
		model = "snapshot:" + im.Snapshot
	} else if model, err = fileSHA256(im.Path); err != nil {
		return
	}
	if d.Slots != "" {
		if slotsSum, err = fileSHA256(d.Slots); err != nil {
			return
		}
	}
	if d.Head != "" {
		head, err = fileSHA256(d.Head)
	}
	return
}

// gateIntent decide si la capa 3 de una tarea puede encenderse.
func (g *Gateway) gateIntent(cfg *Config, name string, tc *TaskConfig) CascadeState {
	d := tc.Intent
	if d.Encoder == "" {
		return CascadeState{Status: "off"}
	}
	st := CascadeState{To: d.Encoder}
	refuse := func(why string) CascadeState {
		if d.EncoderForce {
			st.Status, st.Reason = "forced", "encoder layer "+d.Encoder+" forced (encoder_force) despite: "+why
			return st
		}
		st.Status = "refused"
		st.Reason = fmt.Sprintf("encoder layer %q refused: %s; the fast layers answer and escalate to \"encoder\" instead "+
			"(run `kling ai eval %s -data <held-out.jsonl>`, or set \"encoder_force\": true to enable it anyway)", d.Encoder, why, name)
		return st
	}
	p := g.evalPath(name)
	if p == "" {
		return refuse("no eval record shows the encoder beats the fast layers on this task")
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return refuse("no eval record shows the encoder beats the fast layers on this task")
	}
	var rec IntentEvalRecord
	if err == nil {
		err = json.Unmarshal(b, &rec)
	}
	if err != nil || rec.Kind != "intent" {
		return refuse(fmt.Sprintf("its eval record is unreadable or not an intent eval (%v)", err))
	}
	st.Verdict = rec.Verdict
	if rec.Encoder.Model != d.Encoder || rec.Encoder.Snapshot != cfg.Models[d.Encoder].Snapshot {
		return refuse(fmt.Sprintf("the eval was for %s (%s)", rec.Encoder.Model, rec.Encoder.Snapshot))
	}
	dm, in, sl, hd, err := intentIdentity(cfg, d)
	if err != nil {
		return refuse("reading the task's models: " + err.Error())
	}
	if dm != rec.Domain || in != rec.Intent.SHA256 || sl != rec.SlotsSHA256 || hd != rec.HeadSHA256 || d.FinalOOS != rec.FinalOOS {
		return refuse("the domain, intent, slot or head model (or final_oos) changed since the eval")
	}
	if !rec.BeatsFast {
		return refuse("its eval does not show it beats the fast layers (" + rec.Verdict + ")")
	}
	st.Status = "on"
	return st
}

// evalIntent pasa filas etiquetadas por la cascada sin y con la capa 3. Una
// sola pasada: solo lo que las capas rápidas no contestan confiado va al
// codificador.
func (g *Gateway) evalIntent(ctx context.Context, req EvalRequest) (*IntentEvalRecord, error) {
	t0 := time.Now()
	cfg := g.config()
	tc := cfg.Tasks[req.Task]
	d := tc.Intent
	if d.Encoder == "" {
		return nil, statusf(http.StatusBadRequest, "task %q has no encoder layer to evaluate", req.Task)
	}
	if len(req.Rows) == 0 || len(req.Rows) > maxEvalExamples {
		return nil, statusf(http.StatusBadRequest, "need 1..%d rows (text, lang, intent, slots)", maxEvalExamples)
	}
	fast, err := g.decider(cfg, d, false)
	if err != nil {
		return nil, statusf(http.StatusServiceUnavailable, "%v", err)
	}
	full, err := g.decider(cfg, d, true)
	if err != nil {
		return nil, statusf(http.StatusServiceUnavailable, "%v", err)
	}
	dom := fast.Domain
	dh := sha256.New()
	enc := json.NewEncoder(dh)
	gold := make([]any, len(req.Rows))
	for i := range req.Rows {
		if len(req.Rows[i].Text) > maxText {
			return nil, statusf(http.StatusRequestEntityTooLarge, "row %d: text larger than %d bytes", i+1, maxText)
		}
		if gold[i], err = dom.ParseSlots(req.Rows[i].Slots); err != nil {
			return nil, statusf(http.StatusBadRequest, "row %d: %v", i+1, err)
		}
		_ = enc.Encode(req.Rows[i])
	}
	exact := func(i int, x intent.Decision) bool {
		return dom.Exact(req.Rows[i].Intent, gold[i], x.Intent, x.Slots)
	}
	var res IntentEvalResults
	res.Examples = len(req.Rows)
	var fOK, cOK, fConf, cConf, fConfOK, cConfOK int
	var lat []float64
	for i, r := range req.Rows {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("eval interrupted: %w", err)
		}
		fd := fast.DecideContext(ctx, r.Text, r.Lang)
		cd := fd
		if !fd.Confident {
			cd = full.DecideContext(ctx, r.Text, r.Lang)
			res.ToEncoder++
			switch {
			case cd.EncoderError != "":
				res.EncoderErrors++
			case cd.Layer == intent.LayerEncoder && cd.Confident:
				res.EncoderConfident++
			}
			if cd.EncoderUS > 0 {
				lat = append(lat, cd.EncoderUS/1000)
			}
		}
		fr, cr := exact(i, fd), exact(i, cd)
		if fr {
			fOK++
		}
		if cr {
			cOK++
		}
		if fd.Confident {
			fConf++
			if fr {
				fConfOK++
			} else {
				res.FastConfidentWrong++
			}
		}
		if cd.Confident {
			cConf++
			if cr {
				cConfOK++
			} else {
				res.CascadeConfidentWrong++
			}
		}
		switch {
		case fr && !cr:
			res.ExactFastOnlyRight++
		case cr && !fr:
			res.ExactCascadeOnlyRight++
		}
		fa, ca := fd.Confident && fr, cd.Confident && cr
		switch {
		case fa && !ca:
			res.FastOnlyRight++
		case ca && !fa:
			res.CascadeOnlyRight++
		}
		if (i+1)%2000 == 0 {
			log.Printf("eval %s: %d/%d rows", req.Task, i+1, len(req.Rows))
		}
	}
	n := len(req.Rows)
	res.FastExact, res.CascadeExact = ratio(fOK, n), ratio(cOK, n)
	res.FastCoverage, res.CascadeCoverage = ratio(fConf, n), ratio(cConf, n)
	res.FastPrecision, res.CascadePrecision = ratio(fConfOK, fConf), ratio(cConfOK, cConf)
	res.FastAnswered, res.CascadeAnswered = ratio(fConfOK, n), ratio(cConfOK, n)
	res.PValue = mcnemarOneSided(res.FastOnlyRight, res.CascadeOnlyRight)
	res.ExactPValue = mcnemarOneSided(res.ExactFastOnlyRight, res.ExactCascadeOnlyRight)
	sort.Float64s(lat)
	res.EncoderP50MS, res.EncoderP95MS = quantile(lat, 0.5), quantile(lat, 0.95)
	res.DurationS = math.Round(time.Since(t0).Seconds()*10) / 10

	rec := &IntentEvalRecord{Task: req.Task, Kind: "intent", At: time.Now().UTC().Truncate(time.Second), FinalOOS: d.FinalOOS, Results: res}
	rec.Data.Name, rec.Data.SHA256, rec.Data.Examples = req.Data, hex.EncodeToString(dh.Sum(nil)), n
	rec.Intent.Model = d.Model
	if rec.Domain, rec.Intent.SHA256, rec.SlotsSHA256, rec.HeadSHA256, err = intentIdentity(cfg, d); err != nil {
		return nil, err
	}
	rec.Encoder.Model, rec.Encoder.Snapshot = d.Encoder, cfg.Models[d.Encoder].Snapshot
	// Gana si contesta bien más órdenes donde discrepan (McNemar) y no
	// comete más errores confiados que el 1 % de lo que gana: una capa que
	// contesta más pero se equivoca más con confianza ejecutaría órdenes que
	// nadie pidió.
	gain := res.CascadeOnlyRight - res.FastOnlyRight
	extraWrong := res.CascadeConfidentWrong - res.FastConfidentWrong
	sig := gain > 0 && res.PValue < 0.05
	rec.BeatsFast = sig && extraWrong <= int(math.Ceil(0.01*float64(gain)))
	detail := fmt.Sprintf("answered right %.3f vs %.3f (+%d, McNemar p=%.2g), confident errors %d vs %d; exact with escalated guesses %.3f vs %.3f (p=%.2g), %d rows",
		res.CascadeAnswered, res.FastAnswered, gain, res.PValue, res.CascadeConfidentWrong, res.FastConfidentWrong,
		res.CascadeExact, res.FastExact, res.ExactPValue, n)
	switch {
	case rec.BeatsFast:
		rec.Verdict = "the encoder layer wins: " + detail
	case sig:
		rec.Verdict = "the encoder layer answers more but its confident errors grow too much: " + detail
	case gain > 0:
		rec.Verdict = "not enough evidence for the encoder layer: " + detail
	default:
		rec.Verdict = "the fast layers are as good or better: " + detail
	}
	if req.DryRun {
		return rec, nil
	}
	if err := g.saveRecord(rec.Task, rec); err != nil {
		return nil, err
	}
	rec.Stored = g.evalPath(rec.Task)
	g.regate()
	return rec, nil
}

// saveRecord escribe un registro de evaluación cualquiera (temporal y rename,
// como saveEval).
func (g *Gateway) saveRecord(task string, v any) error {
	p := g.evalPath(task)
	if p == "" {
		return errors.New("this gateway has no directory for eval records")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// captureDecide guarda una orden que las capas rápidas escalaron por culpa de
// la INTENCIÓN (Chispa dudó, o dijo «fuera de ámbito», que es donde caen las
// órdenes indirectas); las que escalaron por un hueco que falta o por ser
// dos órdenes no enseñan nada al modelo de intención. Si la capa del
// codificador contestó confiada, su respuesta va como voto de maestro.
func (g *Gateway) captureDecide(ctx context.Context, ls learnState, cfg *Config, tc *TaskConfig, task, id, text string, d intent.Decision, oos string) {
	in := chispa.Input{Text: text, Fields: map[string]any{"lang": d.Lang}}
	var p chispa.Prediction
	if mc := cfg.Models[tc.Intent.Model]; mc.Backend == BackendMicroVM {
		// La réplica acaba de contestar esta orden (está despierta): se le
		// pide la distribución entera, que sin explain solo manda si duda.
		a, err := g.askChispaGuest(ctx, mc.Snapshot, in, true)
		if err != nil {
			return
		}
		p = a.Pred
		_, p.Confident = guestConfident(p, nil)
	} else {
		im, err := g.chispa.get(mc.Path)
		if err != nil {
			return
		}
		p = im.PredictFull(in, 0)
	}
	if p.Confident && p.Label != oos {
		return
	}
	var votes []TeacherVote
	if d.Layer == intent.LayerEncoder && d.Confident && d.Intent != "" {
		votes = []TeacherVote{{Name: tc.Intent.Encoder, Label: d.Intent, Conf: round4(d.Prob)}}
	}
	g.captureEscalation(ls, task, id, "escalated", in, p, votes)
}
