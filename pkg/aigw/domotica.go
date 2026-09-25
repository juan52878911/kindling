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
	"github.com/juan52878911/kindling/pkg/domotica"
)

// TAREAS DE DOMÓTICA: /v1/decide para la habitación de demo.
//
// La decisión es la cascada de pkg/domotica: plantillas de la demo (capa 1),
// Chispa + Chispa-slots (capa 2, en proceso, microsegundos) y, para lo que esas
// dudan, el codificador de frases (capa 3): una réplica de un dorado kind
// embed que pkg/scheduler despierta con la primera petición y congela al
// quedarse ociosa, igual que un VON, y la cabeza .jenc que clasifica su
// vector aquí mismo. Lo que tampoco resuelve sale con escalate: "von".
//
// La capa 3 se enciende como la cascada Chispa → VON: solo si un registro de
// evaluación (`kling ai eval <tarea> -data test.jsonl`) muestra que la cascada
// con el codificador acierta la orden COMPLETA (intención y huecos) más veces
// que sin él (McNemar, p < 0,05), con los mismos modelos que se sirven. Si no,
// la tarea contesta con las capas rápidas y escala a "encoder".

// DomoticaConfig es una tarea de domótica.
type DomoticaConfig struct {
	// Intent es el modelo Chispa de intención (kind chispa).
	Intent string `json:"intent"`
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

func (c *Config) validateDomotica(n string, t *TaskConfig) []error {
	var errs []error
	d := t.Domotica
	if t.Chispa != "" || t.VON != "" || t.EscalateTo != "" || t.EscalateForce || len(t.Labels) > 0 || t.System != "" ||
		t.Prompt != "" || t.TopK != 0 || len(t.Thresholds) > 0 || t.Precision != 0 || t.Audit != 0 || t.Samples != 0 ||
		t.MaxTokens != 0 || t.Temperature != nil || t.Grammar != nil || t.OnVONError != "" {
		errs = append(errs, fmt.Errorf("task %q: a domotica task takes only its \"domotica\" block", n))
	}
	if m := c.Models[d.Intent]; m == nil || m.Kind != KindChispa {
		errs = append(errs, fmt.Errorf("task %q: domotica.intent %q is not a chispa model", n, d.Intent))
	}
	if (d.Encoder == "") != (d.Head == "") {
		errs = append(errs, fmt.Errorf("task %q: domotica.encoder and domotica.head go together", n))
	}
	if d.Encoder != "" {
		if m := c.Models[d.Encoder]; m == nil || m.Kind != KindEmbed {
			errs = append(errs, fmt.Errorf("task %q: domotica.encoder %q is not an embed model", n, d.Encoder))
		}
	}
	if d.EncoderForce && d.Encoder == "" {
		errs = append(errs, fmt.Errorf("task %q: encoder_force without an encoder", n))
	}
	return errs
}

// ---- modelos de las tareas de domótica

var (
	matcherOnce sync.Once
	matcherVal  *domotica.Matcher
	matcherErr  error
)

func demoMatcher() (*domotica.Matcher, error) {
	matcherOnce.Do(func() { matcherVal, matcherErr = domotica.NewMatcher(domotica.DemoTemplates) })
	return matcherVal, matcherErr
}

// domoFiles guarda los .chispas y .jenc cargados (pocos, y de ~100 KB: sin
// presupuesto). Reload los olvida.
type domoFiles struct {
	mu    sync.Mutex
	slots map[string]*slots.Model
	heads map[string]*codificador.Head
}

func (f *domoFiles) reset() {
	f.mu.Lock()
	f.slots, f.heads = nil, nil
	f.mu.Unlock()
}

// loadSlotsFn y loadHeadFn se sustituyen en los tests.
var (
	loadSlotsFn = slots.LoadFile
	loadHeadFn  = codificador.LoadFile
)

// getSlots y getHead leen el fichero FUERA del candado, como pkg/aigw/chispacache.go:
// una tarea cuyo .chispas o .jenc tarda en leerse (o cuyo disco anda lento) no
// para las decisiones de las demás tareas, que solo tocan el candado para un
// mapa ya en memoria. Dos peticiones que piden a la vez el mismo fichero
// pueden leerlo dos veces (son pocos, de ~100 KB: no compensa la coordinación
// de chispacache), pero la segunda comprobación bajo el candado asegura que solo
// una entrada gana y todo el mundo ve la misma.

func (f *domoFiles) getSlots(p string) (*slots.Model, error) {
	f.mu.Lock()
	m := f.slots[p]
	f.mu.Unlock()
	if m != nil {
		return m, nil
	}
	m, err := loadSlotsFn(p)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing := f.slots[p]; existing != nil {
		return existing, nil
	}
	if f.slots == nil {
		f.slots = map[string]*slots.Model{}
	}
	f.slots[p] = m
	return m, nil
}

func (f *domoFiles) getHead(p string) (*codificador.Head, error) {
	f.mu.Lock()
	h := f.heads[p]
	f.mu.Unlock()
	if h != nil {
		return h, nil
	}
	h, err := loadHeadFn(p)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing := f.heads[p]; existing != nil {
		return existing, nil
	}
	if f.heads == nil {
		f.heads = map[string]*codificador.Head{}
	}
	f.heads[p] = h
	return h, nil
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

// decider arma la cascada de una tarea. withEncoder añade la capa 3 (si la
// tarea la tiene), esté o no encendida: la evaluación la necesita apagada y
// encendida.
func (g *Gateway) decider(cfg *Config, d *DomoticaConfig, withEncoder bool) (*domotica.Decider, error) {
	mt, err := demoMatcher()
	if err != nil {
		return nil, err
	}
	im, err := g.chispa.get(cfg.Models[d.Intent].Path)
	if err != nil {
		return nil, fmt.Errorf("intent model: %w", err)
	}
	dec := &domotica.Decider{Matcher: mt, Intent: im, FinalOOS: d.FinalOOS}
	if d.Slots != "" {
		if dec.Slots, err = g.domo.getSlots(d.Slots); err != nil {
			return nil, fmt.Errorf("slot model: %w", err)
		}
	}
	if withEncoder && d.Encoder != "" {
		h, err := g.domo.getHead(d.Head)
		if err != nil {
			return nil, fmt.Errorf("encoder head: %w", err)
		}
		dec.Encoder = &codificador.Layer{Head: h, Timeout: g.opts.EncoderTimeout,
			Embedder: replicaEmbedder{g: g, snap: cfg.Models[d.Encoder].Snapshot, dim: h.Dim}}
	}
	return dec, nil
}

// DecideResponse es la respuesta de /v1/decide en una tarea de domótica: la
// decisión de pkg/domotica, con la intención también como "decision" (lo que
// devuelve /v1/decide en las tareas de clasificación).
type DecideResponse struct {
	// ID identifica la respuesta para /v1/feedback; solo en tareas con "learn".
	ID    string `json:"id,omitempty"`
	Task  string `json:"task"`
	Label string `json:"decision"`
	domotica.Decision
	LatencyMS float64 `json:"latency_ms"`
	// Encoder dice si la capa 3 estaba encendida y, si no, por qué (el estado
	// de su evaluación): quien llama no tiene que adivinarlo.
	Encoder *CascadeState `json:"encoder,omitempty"`
}

// Decide decide una orden de una tarea de domótica.
func (g *Gateway) Decide(ctx context.Context, req ClassifyRequest) (*DecideResponse, error) {
	t0 := time.Now()
	cfg := g.config()
	tc := cfg.Tasks[req.Task]
	if tc == nil {
		return nil, statusf(http.StatusNotFound, "unknown task %q", req.Task)
	}
	if tc.Domotica == nil {
		return nil, statusf(http.StatusBadRequest, "task %q is not a domotica task", req.Task)
	}
	if len(req.Text) > maxText {
		return nil, statusf(http.StatusRequestEntityTooLarge, "text larger than %d bytes", maxText)
	}
	switch req.Lang {
	case "", "auto", "es", "en":
	default:
		return nil, statusf(http.StatusBadRequest, "lang must be es, en or auto")
	}
	casc := g.cascade(req.Task)
	dec, err := g.decider(cfg, tc.Domotica, casc.On())
	if err != nil {
		log.Printf("task %s: %v", req.Task, err)
		return nil, statusf(http.StatusServiceUnavailable, "task %q: %v", req.Task, err)
	}
	d := dec.DecideContext(ctx, req.Text, req.Lang)
	if d.EncoderError != "" {
		g.met.vonErr(tc.Domotica.Encoder, "encoder")
		g.met.inc(g.met.degraded, req.Task)
		log.Printf("task %s: encoder %s: %s", req.Task, tc.Domotica.Encoder, d.EncoderError)
	}
	resp := &DecideResponse{Task: req.Task, Label: d.Intent, Decision: d}
	if tc.Domotica.Encoder != "" {
		resp.Encoder = &casc
	}
	if ls, ok := g.learnFor(req.Task); ok {
		resp.ID = g.learn.newID()
		fast := d.Confident && (d.Layer == domotica.LayerTemplate || d.Layer == domotica.LayerChispa)
		g.met.learnAnswer(req.Task, ls.version, fast, ls.cfg.WindowMinutes)
		if !fast && d.Layer != domotica.LayerTemplate && ls.cfg.capturing() {
			g.captureDecide(ls, cfg, tc, req.Task, resp.ID, req.Text, d)
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

// DomoticaEvalResults son las cifras de la evaluación de una tarea de
// domótica: la cascada sin la capa 3 (plantillas → Chispa) y con ella.
type DomoticaEvalResults struct {
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
	// Lo mismo contando también la conjetura de lo que escala (el «exact» de
	// docs/DOMOTICA-EVAL.md): informativo.
	ExactFastOnlyRight    int     `json:"exact_fast_only_right"`
	ExactCascadeOnlyRight int     `json:"exact_cascade_only_right"`
	ExactPValue           float64 `json:"exact_p_value"`
	EncoderP50MS          float64 `json:"encoder_p50_ms"`
	EncoderP95MS          float64 `json:"encoder_p95_ms"`
	DurationS             float64 `json:"duration_s"`
}

// DomoticaEvalRecord es lo que se guarda (ai-evals/<tarea>.json) y lo que
// mira la puerta: la identidad de todo lo que cambia el resultado.
type DomoticaEvalRecord struct {
	Task string    `json:"task"`
	Kind string    `json:"kind"` // "domotica"
	At   time.Time `json:"at"`
	Data struct {
		Name     string `json:"name,omitempty"`
		SHA256   string `json:"sha256"`
		Examples int    `json:"examples"`
	} `json:"data"`
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
	FinalOOS  bool                `json:"final_oos"`
	Results   DomoticaEvalResults `json:"results"`
	BeatsFast bool                `json:"beats_fast"`
	Verdict   string              `json:"verdict"`
	Stored    string              `json:"stored,omitempty"`
}

// domoIdentity son los hashes de lo que sirve una tarea de domótica.
func domoIdentity(cfg *Config, d *DomoticaConfig) (intent, slotsSum, head string, err error) {
	if intent, err = fileSHA256(cfg.Models[d.Intent].Path); err != nil {
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

// gateDomotica decide si la capa 3 de una tarea puede encenderse.
func (g *Gateway) gateDomotica(cfg *Config, name string, tc *TaskConfig) CascadeState {
	d := tc.Domotica
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
	var rec DomoticaEvalRecord
	if err == nil {
		err = json.Unmarshal(b, &rec)
	}
	if err != nil || rec.Kind != "domotica" {
		return refuse(fmt.Sprintf("its eval record is unreadable or not a domotica eval (%v)", err))
	}
	st.Verdict = rec.Verdict
	if rec.Encoder.Model != d.Encoder || rec.Encoder.Snapshot != cfg.Models[d.Encoder].Snapshot {
		return refuse(fmt.Sprintf("the eval was for %s (%s)", rec.Encoder.Model, rec.Encoder.Snapshot))
	}
	in, sl, hd, err := domoIdentity(cfg, d)
	if err != nil {
		return refuse("reading the task's models: " + err.Error())
	}
	if in != rec.Intent.SHA256 || sl != rec.SlotsSHA256 || hd != rec.HeadSHA256 || d.FinalOOS != rec.FinalOOS {
		return refuse("the intent, slot or head model (or final_oos) changed since the eval")
	}
	if !rec.BeatsFast {
		return refuse("its eval does not show it beats the fast layers (" + rec.Verdict + ")")
	}
	st.Status = "on"
	return st
}

// evalDomotica pasa filas etiquetadas por la cascada sin y con la capa 3. Una
// sola pasada: solo lo que las capas rápidas no contestan confiado va al
// codificador.
func (g *Gateway) evalDomotica(ctx context.Context, req EvalRequest) (*DomoticaEvalRecord, error) {
	t0 := time.Now()
	cfg := g.config()
	tc := cfg.Tasks[req.Task]
	d := tc.Domotica
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
	dh := sha256.New()
	enc := json.NewEncoder(dh)
	for i := range req.Rows {
		if len(req.Rows[i].Text) > maxText {
			return nil, statusf(http.StatusRequestEntityTooLarge, "row %d: text larger than %d bytes", i+1, maxText)
		}
		_ = enc.Encode(req.Rows[i])
	}
	exact := func(r domotica.Row, x domotica.Decision) bool {
		return x.Intent == r.Intent && domotica.SlotsEqual(domotica.Resolve(r.Intent, r.Slots), domotica.Resolve(x.Intent, x.Slots))
	}
	var res DomoticaEvalResults
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
			case cd.Layer == domotica.LayerEncoder && cd.Confident:
				res.EncoderConfident++
			}
			if cd.EncoderUS > 0 {
				lat = append(lat, cd.EncoderUS/1000)
			}
		}
		fr, cr := exact(r, fd), exact(r, cd)
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

	rec := &DomoticaEvalRecord{Task: req.Task, Kind: "domotica", At: time.Now().UTC().Truncate(time.Second), FinalOOS: d.FinalOOS, Results: res}
	rec.Data.Name, rec.Data.SHA256, rec.Data.Examples = req.Data, hex.EncodeToString(dh.Sum(nil)), n
	rec.Intent.Model = d.Intent
	if rec.Intent.SHA256, rec.SlotsSHA256, rec.HeadSHA256, err = domoIdentity(cfg, d); err != nil {
		return nil, err
	}
	rec.Encoder.Model, rec.Encoder.Snapshot = d.Encoder, cfg.Models[d.Encoder].Snapshot
	// Gana si contesta bien más órdenes donde discrepan (McNemar) y no
	// comete más errores confiados que el 1 % de lo que gana: una capa que
	// contesta más pero se equivoca más con confianza haría cosas en la
	// habitación que nadie pidió.
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
func (g *Gateway) captureDecide(ls learnState, cfg *Config, tc *TaskConfig, task, id, text string, d domotica.Decision) {
	im, err := g.chispa.get(cfg.Models[tc.Domotica.Intent].Path)
	if err != nil {
		return
	}
	in := chispa.Input{Text: text, Fields: map[string]any{"lang": d.Lang}}
	p := im.PredictFull(in, 0)
	if p.Confident && p.Label != domotica.OutOfScope {
		return
	}
	var votes []TeacherVote
	if d.Layer == domotica.LayerEncoder && d.Confident && d.Intent != "" {
		votes = []TeacherVote{{Name: tc.Domotica.Encoder, Label: d.Intent, Conf: round4(d.Prob)}}
	}
	g.captureEscalation(ls, task, id, "escalated", in, p, votes)
}
