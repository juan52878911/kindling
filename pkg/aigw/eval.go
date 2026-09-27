package aigw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/chispa"
	"github.com/juan52878911/kindling/pkg/intent"
)

// LA CASCADA, RESPALDADA POR DATOS.
//
// Medido en la clasificación de commits (docs/ai-gateway.md): Chispa solo acertaba
// el 64 %, y la cascada Chispa → LLM de 0,5B el 43 %. Un LLM pequeño con un prompt
// de pocos ejemplos no mejora a un clasificador entrenado para la tarea por el
// mero hecho de ser un LLM; en lo que Chispa duda, acertaba menos que Chispa. Así que
// la cascada no se activa porque sí: `kling ai eval <tarea> -data test.jsonl`
// pasa un conjunto etiquetado por Chispa y por la cascada con el VON candidato,
// guarda el resultado junto al registro (ai-evals/<tarea>.json) y el gateway
// solo activa escalate_to si ese registro dice que la cascada gana, con los
// mismos modelos y los mismos ajustes que se van a servir. Si no, se niega (y
// dice por qué) salvo escalate_force.
//
// "Gana" no es "sacó una décima más": la cascada y Chispa solo contestan lo mismo
// en todo lo que Chispa no escala, así que la diferencia sale entera de lo
// escalado. Se compara con la prueba de McNemar sobre las discrepancias (Chispa
// acierta y la cascada no, o al revés), exacta y de una cola: gana si acierta
// más veces donde discrepan y p < 0,05. Con pocos datos no se puede demostrar
// nada, y eso también es una respuesta.

// EvalExample es un ejemplo etiquetado.
type EvalExample struct {
	Text   string         `json:"text"`
	Fields map[string]any `json:"fields,omitempty"`
	Label  string         `json:"label"`
}

// EvalRequest es la petición de `kling ai eval`.
type EvalRequest struct {
	Task string `json:"task"`
	// VON es el modelo candidato; vacío = el escalate_to de la tarea.
	VON string `json:"von,omitempty"`
	// Data es el nombre del conjunto, para el registro (la ruta del CLI).
	Data     string        `json:"data,omitempty"`
	Examples []EvalExample `json:"examples"`
	// Concurrency son las escaladas en vuelo a la vez (1 por defecto; más
	// despierta más réplicas, hasta el tope del modelo).
	Concurrency int `json:"concurrency,omitempty"`
	// VONAlone mide además a VON solo, con todas las etiquetas y sin pistas
	// de Chispa, en TODOS los ejemplos: informativo, no decide nada.
	VONAlone bool `json:"von_alone,omitempty"`
	// DryRun no guarda el registro.
	DryRun bool `json:"dry_run,omitempty"`
	// Rows son las filas etiquetadas de una tarea de intención (texto,
	// idioma, intención y huecos): la orden entera, no solo una etiqueta.
	Rows []intent.Row `json:"rows,omitempty"`
}

// EvalResults son las cifras.
type EvalResults struct {
	Examples          int     `json:"examples"`
	UnseenLabels      int     `json:"unseen_labels"` // ejemplos con una etiqueta que el modelo no conoce
	ChispaAccuracy    float64 `json:"chispa_accuracy"`
	CascadeAccuracy   float64 `json:"cascade_accuracy"`
	Coverage          float64 `json:"coverage"`           // fracción que Chispa contesta confiado
	ConfidentAccuracy float64 `json:"confident_accuracy"` // acierto de Chispa en eso
	Escalated         int     `json:"escalated"`
	// En lo escalado: cuánto acierta cada uno.
	ChispaAccuracyEscalated float64 `json:"chispa_accuracy_escalated"`
	VONAccuracyEscalated    float64 `json:"von_accuracy_escalated"`
	VONUnknown              int     `json:"von_unknown"`
	VONErrors               int     `json:"von_errors"`
	// Discrepancias: Chispa acierta y la cascada no (b), y al revés (c).
	ChispaOnlyRight int     `json:"chispa_only_right"`
	VONOnlyRight    int     `json:"cascade_only_right"`
	PValue          float64 `json:"p_value"` // McNemar exacta, una cola: P(X >= c | b+c, 1/2)
	// VON solo en todos los ejemplos (von_alone).
	VONAloneAccuracy *float64 `json:"von_alone_accuracy,omitempty"`
	VONLatencyP50MS  float64  `json:"von_latency_p50_ms"`
	VONLatencyP95MS  float64  `json:"von_latency_p95_ms"`
	DurationS        float64  `json:"duration_s"`
}

// EvalRecord es lo que se guarda con la tarea y lo que mira el gateway para
// activar la cascada. Lleva la identidad de todo lo que cambia el resultado:
// si algo de eso cambia, el registro deja de valer.
type EvalRecord struct {
	Task string    `json:"task"`
	At   time.Time `json:"at"`
	Data struct {
		Name     string `json:"name,omitempty"`
		SHA256   string `json:"sha256"`
		Examples int    `json:"examples"`
	} `json:"data"`
	Chispa struct {
		Model  string `json:"model"`
		SHA256 string `json:"sha256"` // del .chispa: reentrenar o recalibrar lo invalida
	} `json:"chispa"`
	VON struct {
		Model    string `json:"model"`
		Snapshot string `json:"snapshot"`
	} `json:"von"`
	// Settings es el hash de lo que cambia la pregunta a VON y qué se escala:
	// system, prompt, top_k, gramática, max_tokens y umbrales forzados.
	Settings    string      `json:"settings_sha256"`
	Results     EvalResults `json:"results"`
	BeatsChispa bool        `json:"beats_chispa"`
	Verdict     string      `json:"verdict"`
	// Stored es dónde se guardó (vacío en seco).
	Stored string `json:"stored,omitempty"`
}

// maxEvalExamples acota un conjunto de evaluación.
const maxEvalExamples = 100_000

// settingsHash resume los ajustes de la escalada de una tarea. Con los valores
// por defecto ya puestos: escribir "grammar": true donde no estaba no cambia
// nada y no debe invalidar una evaluación.
func settingsHash(tc *TaskConfig) string {
	gram := tc.Grammar == nil || *tc.Grammar
	mt := tc.MaxTokens
	if mt == 0 {
		mt = 16
	}
	sys := tc.System
	if sys == "" {
		sys = defaultSystem
	}
	pr := tc.Prompt
	if pr == "" {
		pr = defaultPrompt
	}
	b, _ := json.Marshal(struct {
		System, Prompt string
		TopK           int
		Grammar        bool
		MaxTokens      int
		Thresholds     map[string]float64
		OnVONError     string
	}{sys, pr, tc.TopK, gram, mt, tc.Thresholds, tc.OnVONError})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// fileSHA256 es el hash de un fichero (un .chispa: pocos MB).
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// evalPath es dónde vive el registro de evaluación de una tarea. Los nombres de
// tarea pasan nameRE: no pueden salir del directorio.
func (g *Gateway) evalPath(task string) string {
	if g.opts.EvalDir == "" {
		return ""
	}
	return filepath.Join(g.opts.EvalDir, task+".json")
}

// LoadEval lee el registro de evaluación de una tarea (nil si no hay).
func (g *Gateway) LoadEval(task string) (*EvalRecord, error) {
	p := g.evalPath(task)
	if p == "" {
		return nil, nil
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r EvalRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &r, nil
}

// CascadeState es si la cascada de una tarea está activa y por qué.
type CascadeState struct {
	// Status: "off" (sin escalate_to), "on" (respaldada por su evaluación),
	// "forced" (escalate_force) o "refused" (escalate_to sin respaldo: Chispa
	// contesta y marca escalate: true).
	Status string `json:"status"`
	To     string `json:"to,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Verdict es el de la evaluación que se tuvo en cuenta, si la hay.
	Verdict string `json:"eval,omitempty"`
}

// On dice si lo que Chispa duda va a VON.
func (c CascadeState) On() bool { return c.Status == "on" || c.Status == "forced" }

// gate decide si la cascada de una tarea puede activarse.
func (g *Gateway) gate(cfg *Config, name string, tc *TaskConfig) CascadeState {
	if tc.Intent != nil {
		return g.gateIntent(cfg, name, tc)
	}
	if tc.EscalateTo == "" || tc.Chispa == "" {
		return CascadeState{Status: "off"}
	}
	st := CascadeState{To: tc.EscalateTo}
	refuse := func(why string) CascadeState {
		if tc.EscalateForce {
			st.Status, st.Reason = "forced", "cascade to "+tc.EscalateTo+" forced (escalate_force) despite: "+why
			return st
		}
		st.Status = "refused"
		st.Reason = fmt.Sprintf("escalate_to %q refused: %s; Chispa answers with escalate: true instead "+
			"(run `kling ai eval %s -data <held-out.jsonl>`, or set \"escalate_force\": true to enable it anyway)",
			tc.EscalateTo, why, name)
		return st
	}
	rec, err := g.LoadEval(name)
	switch {
	case err != nil:
		return refuse("its eval record is unreadable: " + err.Error())
	case rec == nil:
		return refuse("no eval record shows the cascade beats Chispa alone on this task")
	}
	st.Verdict = rec.Verdict
	vm := cfg.Models[tc.EscalateTo]
	if rec.VON.Model != tc.EscalateTo || rec.VON.Snapshot != vm.Snapshot {
		return refuse(fmt.Sprintf("the eval was for %s (%s), not %s (%s)", rec.VON.Model, rec.VON.Snapshot, tc.EscalateTo, vm.Snapshot))
	}
	sum, err := fileSHA256(cfg.Models[tc.Chispa].Path)
	if err != nil {
		return refuse("reading the chispa model: " + err.Error())
	}
	if sum != rec.Chispa.SHA256 {
		return refuse("the chispa model changed since the eval (retrained or recalibrated)")
	}
	if settingsHash(tc) != rec.Settings {
		return refuse("the task's escalation settings (system, prompt, top_k, grammar, max_tokens, thresholds or on_von_error) changed since the eval")
	}
	if !rec.BeatsChispa {
		return refuse("its eval does not show it beats Chispa alone (" + rec.Verdict + ")")
	}
	st.Status = "on"
	return st
}

// gates calcula el estado de la cascada de todas las tareas. Lee ficheros: se
// llama sin candados.
func (g *Gateway) gates(cfg *Config) map[string]CascadeState {
	out := map[string]CascadeState{}
	for _, n := range sortedKeys(cfg.Tasks) {
		out[n] = g.gate(cfg, n, cfg.Tasks[n])
	}
	return out
}

// regate recalcula las cascadas con el registro en uso (tras guardar una
// evaluación o reescribir un modelo Chispa) y devuelve las notas.
func (g *Gateway) regate() []string {
	cfg := g.config()
	st := g.gates(cfg)
	g.cfgMu.Lock()
	if g.cfg == cfg { // si entretanto se recargó el registro, ya lleva lo suyo
		g.cascades = st
	}
	g.cfgMu.Unlock()
	return CascadeNotes(st)
}

// CascadeNotes son las líneas que merece la pena decir de unas cascadas: las
// activas, las forzadas y las rechazadas.
func CascadeNotes(st map[string]CascadeState) []string {
	var out []string
	for _, n := range sortedKeys(st) {
		switch s := st[n]; s.Status {
		case "refused", "forced":
			out = append(out, fmt.Sprintf("task %s: %s", n, s.Reason))
		case "on":
			out = append(out, fmt.Sprintf("task %s: cascade to %s on (%s)", n, s.To, s.Verdict))
		}
	}
	return out
}

// Cascades es el estado de la cascada de cada tarea.
func (g *Gateway) Cascades() map[string]CascadeState {
	g.cfgMu.RLock()
	defer g.cfgMu.RUnlock()
	out := make(map[string]CascadeState, len(g.cascades))
	for k, v := range g.cascades {
		out[k] = v
	}
	return out
}

func (g *Gateway) cascade(task string) CascadeState {
	g.cfgMu.RLock()
	defer g.cfgMu.RUnlock()
	return g.cascades[task]
}

// Eval pasa un conjunto etiquetado por Chispa solo y por la cascada con el VON
// candidato, y guarda el registro con la tarea. Una sola pasada: Chispa cuesta
// microsegundos y solo lo escalado se pregunta a VON.
func (g *Gateway) Eval(ctx context.Context, req EvalRequest) (*EvalRecord, error) {
	t0 := time.Now()
	cfg := g.config()
	tc := cfg.Tasks[req.Task]
	if tc == nil {
		return nil, statusf(http.StatusNotFound, "unknown task %q", req.Task)
	}
	if tc.Chispa == "" {
		return nil, statusf(http.StatusBadRequest, "task %q is a generation task: there is no cascade to evaluate", req.Task)
	}
	vonName := req.VON
	if vonName == "" {
		vonName = tc.EscalateTo
	}
	vm := cfg.Models[vonName]
	if vonName == "" || vm == nil || vm.Kind != KindVON {
		return nil, statusf(http.StatusBadRequest, "name the candidate von model (-von); %q is not one", vonName)
	}
	if len(req.Examples) == 0 || len(req.Examples) > maxEvalExamples {
		return nil, statusf(http.StatusBadRequest, "need 1..%d examples", maxEvalExamples)
	}
	conc := min(max(req.Concurrency, 1), 16)
	path := cfg.Models[tc.Chispa].Path
	m, err := g.chispa.get(path)
	if err != nil {
		return nil, statusf(http.StatusServiceUnavailable, "chispa model %q unavailable: %v", tc.Chispa, err)
	}
	chispaSum, err := fileSHA256(path)
	if err != nil {
		return nil, err
	}

	// El hash del conjunto: el mismo registro con otros datos no es la misma
	// evaluación, y así se puede comprobar sobre qué se midió.
	dh := sha256.New()
	enc := json.NewEncoder(dh)
	for i := range req.Examples {
		ex := &req.Examples[i]
		if len(ex.Text) > maxText {
			return nil, statusf(http.StatusRequestEntityTooLarge, "example %d: text larger than %d bytes", i+1, maxText)
		}
		_ = enc.Encode(ex)
	}

	n := len(req.Examples)
	preds := make([]chispa.Prediction, n)
	confident := make([]bool, n)
	var escalated []int
	for i, ex := range req.Examples {
		p, _, ok := chispaDecide(m, tc, chispa.Input{Text: ex.Text, Fields: ex.Fields}, true)
		preds[i], confident[i] = p, ok
		if !ok {
			escalated = append(escalated, i)
		}
	}

	// Las preguntas a VON, con `conc` en vuelo. vonLabel[i] es lo que habría
	// devuelto la cascada en un escalado ("" = VON no contestó).
	vonLabel := make([]string, n)
	aloneLabel := make([]string, n)
	var lat []float64
	var latMu sync.Mutex
	ask := func(i int, alone bool) {
		ex := req.Examples[i]
		in := chispa.Input{Text: ex.Text, Fields: ex.Fields}
		allowed, chat := g.escalation(tc, m.Labels, in, preds[i])
		if alone {
			allowed = m.Labels
			chat = g.chatFor(tc, allowed, in, nil)
		}
		cctx, cancel := context.WithTimeout(ctx, g.opts.VONTimeout)
		defer cancel()
		t := time.Now()
		ans, err := g.askVON(cctx, vm.Snapshot, chat)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("eval %s: von %s: %v", req.Task, vonName, err)
			}
			return
		}
		l := parseLabel(ans, allowed)
		if alone {
			aloneLabel[i] = l
			return
		}
		vonLabel[i] = l
		latMu.Lock()
		lat = append(lat, float64(time.Since(t).Microseconds())/1000)
		latMu.Unlock()
	}
	run := func(idx []int, alone bool) {
		work := make(chan int)
		var wg sync.WaitGroup
		for w := 0; w < conc; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range work {
					ask(i, alone)
				}
			}()
		}
	enviar:
		for k, i := range idx {
			select {
			case work <- i:
			case <-ctx.Done():
				break enviar
			}
			if (k+1)%100 == 0 {
				log.Printf("eval %s: %d/%d sent to %s", req.Task, k+1, len(idx), vonName)
			}
		}
		close(work)
		wg.Wait()
	}
	log.Printf("eval %s: %d examples, %d escalated to %s (concurrency %d)", req.Task, n, len(escalated), vonName, conc)
	run(escalated, false)
	if req.VONAlone && ctx.Err() == nil {
		all := make([]int, n)
		for i := range all {
			all[i] = i
		}
		run(all, true)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("eval interrupted: %w", err)
	}

	// Las cuentas.
	var res EvalResults
	res.Examples = n
	var chispaOK, casOK, confN, confOK, escChispa, escVON, aloneOK int
	for i, ex := range req.Examples {
		if m.GoldIndex(ex.Label) < 0 {
			res.UnseenLabels++
		}
		jl := preds[i].Label
		cl := jl
		if !confident[i] {
			switch vl := vonLabel[i]; vl {
			case "":
				res.VONErrors++
				if tc.OnVONError == "error" {
					cl = "" // la cascada habría contestado 503: no acierta
				}
			case Unknown:
				res.VONUnknown++
				cl = Unknown
			default:
				cl = vl
			}
			if jl == ex.Label {
				escChispa++
			}
			if vonLabel[i] == ex.Label {
				escVON++
			}
		} else {
			confN++
			if jl == ex.Label {
				confOK++
			}
		}
		jr, cr := jl == ex.Label, cl == ex.Label
		if jr {
			chispaOK++
		}
		if cr {
			casOK++
		}
		switch {
		case jr && !cr:
			res.ChispaOnlyRight++
		case cr && !jr:
			res.VONOnlyRight++
		}
		if aloneLabel[i] == ex.Label {
			aloneOK++
		}
	}
	res.Escalated = len(escalated)
	res.ChispaAccuracy = ratio(chispaOK, n)
	res.CascadeAccuracy = ratio(casOK, n)
	res.Coverage = ratio(confN, n)
	res.ConfidentAccuracy = ratio(confOK, confN)
	res.ChispaAccuracyEscalated = ratio(escChispa, len(escalated))
	res.VONAccuracyEscalated = ratio(escVON, len(escalated))
	if req.VONAlone {
		a := ratio(aloneOK, n)
		res.VONAloneAccuracy = &a
	}
	res.PValue = mcnemarOneSided(res.ChispaOnlyRight, res.VONOnlyRight)
	sort.Float64s(lat)
	res.VONLatencyP50MS, res.VONLatencyP95MS = quantile(lat, 0.5), quantile(lat, 0.95)
	res.DurationS = math.Round(time.Since(t0).Seconds()*10) / 10

	rec := &EvalRecord{Task: req.Task, At: time.Now().UTC().Truncate(time.Second), Settings: settingsHash(tc), Results: res}
	rec.Data.Name, rec.Data.SHA256, rec.Data.Examples = req.Data, hex.EncodeToString(dh.Sum(nil)), n
	rec.Chispa.Model, rec.Chispa.SHA256 = tc.Chispa, chispaSum
	rec.VON.Model, rec.VON.Snapshot = vonName, vm.Snapshot
	rec.BeatsChispa = res.VONOnlyRight > res.ChispaOnlyRight && res.PValue < 0.05
	switch {
	case rec.BeatsChispa:
		rec.Verdict = fmt.Sprintf("cascade %.3f vs Chispa alone %.3f on %d examples (McNemar p=%.2g): the cascade wins",
			res.CascadeAccuracy, res.ChispaAccuracy, n, res.PValue)
	case res.VONOnlyRight > res.ChispaOnlyRight:
		rec.Verdict = fmt.Sprintf("cascade %.3f vs Chispa alone %.3f on %d examples, not significant (McNemar p=%.2g): not enough evidence",
			res.CascadeAccuracy, res.ChispaAccuracy, n, res.PValue)
	default:
		rec.Verdict = fmt.Sprintf("cascade %.3f vs Chispa alone %.3f on %d examples: Chispa alone is as good or better",
			res.CascadeAccuracy, res.ChispaAccuracy, n)
	}
	if req.DryRun {
		return rec, nil
	}
	if err := g.saveEval(rec); err != nil {
		return nil, err
	}
	g.regate()
	return rec, nil
}

// saveEval escribe el registro de una tarea (fichero temporal y rename: un
// gateway que lo leyera a medias vería un JSON roto y rechazaría la cascada).
func (g *Gateway) saveEval(rec *EvalRecord) error {
	p := g.evalPath(rec.Task)
	if p == "" {
		return errors.New("this gateway has no directory for eval records")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
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
	rec.Stored = p
	return nil
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return math.Round(float64(a)/float64(b)*10000) / 10000
}

func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	return math.Round(sorted[max(i, 0)]*10) / 10
}

// mcnemarOneSided es la prueba de McNemar exacta de una cola: la probabilidad
// de ver c o más aciertos solo-de-la-cascada entre b+c discrepancias si los dos
// fueran igual de buenos (binomial con p = 1/2). En logaritmos: con miles de
// discrepancias los combinatorios no caben en un float64.
func mcnemarOneSided(b, c int) float64 {
	n := b + c
	if n == 0 {
		return 1
	}
	lg := func(x int) float64 { v, _ := math.Lgamma(float64(x) + 1); return v }
	var p float64
	for k := c; k <= n; k++ {
		p += math.Exp(lg(n) - lg(k) - lg(n-k) - float64(n)*math.Ln2)
	}
	return math.Min(1, p)
}
