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
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/chispa"
	"github.com/juan52878911/kindling/pkg/chispa/train"
	"github.com/juan52878911/kindling/pkg/domotica"
)

// REENTRENO CON PUERTA Y VERSIONES.
//
// `kling ai retrain <tarea>` junta oro + etiquetas humanas + lo aceptado de
// maestros validados (learn_review.go), entrena un Chispa SOMBRA con los mismos
// hiperparámetros y la misma especificación que el que se sirve, y mide los
// dos en el conjunto apartado de confianza (learn.heldout). Promociona solo si
// la sombra gana ahí: contesta bien (confiada y acertada) más ejemplos donde
// discrepan, con McNemar de una cola p < 0,05 (o, con la regla
// "no-regression", sin contestar bien menos ni acertar menos), y su precisión
// en lo confiado no baja más que la tolerancia. Es la misma forma de puerta
// que la de la capa del codificador en domótica (domotica.go).
//
// La promoción es atómica y versionada. En proceso: el .chispa nuevo se
// guarda como ai-data/<tarea>/versions/<modelo>@vN.chispa, el que se servía
// queda en <ruta>.prev y la ruta del registro se sustituye con un rename (un
// lector nunca ve medio fichero). Con backend microvm el gateway no puede
// hornear una imagen: deja la versión PENDIENTE y `kling ai retrain` (el CLI)
// hace el dorado <snapshot>-vN con `kling chispa deploy` y lo confirma con
// /v1/admin/promote, que comprueba que el sha256 del dorado es el de la
// versión. La tarea pasa a apuntar a ese dorado (versions.json manda sobre el
// "snapshot" del registro) y el anterior sigue ahí para `kling ai rollback`.
//
// El registro de la cascada (eval.go) va atado al sha256 del .chispa: un
// modelo nuevo apaga una cascada respaldada hasta que se vuelva a evaluar. El
// reentreno lo hace explícito: si la cascada estaba activa, vuelve a correr
// `kling ai eval` con learn.heldout (o con los datos que se le pasen) y dice
// cómo queda; si no puede (VON caído, domótica sin filas), lo dice.

// HeldoutMetrics es un modelo medido en el conjunto de confianza.
type HeldoutMetrics struct {
	Version            string  `json:"version,omitempty"`
	SHA256             string  `json:"sha256,omitempty"`
	Examples           int     `json:"examples"`
	Accuracy           float64 `json:"accuracy"`
	Coverage           float64 `json:"coverage"`
	ConfidentPrecision float64 `json:"confident_precision"`
	AnsweredRight      float64 `json:"answered_right"` // confiado y acertado, sobre todo
	ConfidentErrors    int     `json:"confident_errors"`
}

// RetrainData cuenta lo que entró en un reentreno.
type RetrainData struct {
	Gold      int `json:"gold"`
	Human     int `json:"human"`
	Accepted  int `json:"accepted"`
	Pending   int `json:"pending"`
	Discarded int `json:"discarded"`
	Capped    int `json:"capped"`
	Leaked    int `json:"leaked"` // quitados por estar en el conjunto de confianza
	Train     int `json:"train"`
	Valid     int `json:"valid"`
}

// RetrainGate es la decisión.
type RetrainGate struct {
	Rule               string  `json:"rule"`
	ShadowOnlyRight    int     `json:"shadow_only_right"`
	CurrentOnlyRight   int     `json:"current_only_right"`
	PValue             float64 `json:"p_value"`
	PrecisionTolerance float64 `json:"precision_tolerance"`
	Significant        bool    `json:"significant"`
	NoRegression       bool    `json:"no_regression"`
	PrecisionOK        bool    `json:"precision_ok"`
	Pass               bool    `json:"pass"`
	Verdict            string  `json:"verdict"`
}

// RetrainRequest es la petición de `kling ai retrain`.
type RetrainRequest struct {
	Task   string `json:"task"`
	DryRun bool   `json:"dry_run,omitempty"`
	// Trust fuerza maestros sin validar para este reentreno (queda en el
	// informe y en el registro del reentreno).
	Trust []string `json:"trust_teachers,omitempty"`
	// Rule: "mcnemar" (por defecto) o "no-regression".
	Rule               string   `json:"rule,omitempty"`
	PrecisionTolerance *float64 `json:"precision_tolerance,omitempty"` // 0,005
	// CurrentModel es el .chispa que sirve hoy una tarea microvm que aún no
	// tiene versiones: el gateway no puede leerlo de la réplica. Se comprueba
	// contra el sha256 del registro de despliegue del dorado.
	CurrentModel []byte `json:"current_model,omitempty"`
	// Datos para volver a evaluar la cascada tras promocionar (vacío = el
	// learn.heldout en una tarea de clasificación; en domótica hacen falta
	// filas). NoEval lo salta: la cascada queda apagada hasta evaluarla.
	EvalExamples []EvalExample  `json:"eval_examples,omitempty"`
	EvalRows     []domotica.Row `json:"eval_rows,omitempty"`
	NoEval       bool           `json:"no_eval,omitempty"`
	Seed         uint64         `json:"seed,omitempty"`
}

// RetrainReport es lo que se midió y lo que se hizo.
type RetrainReport struct {
	Task     string         `json:"task"`
	Model    string         `json:"model"`
	At       time.Time      `json:"at"`
	Data     RetrainData    `json:"data"`
	Reasons  map[string]int `json:"pending_reasons,omitempty"`
	Teachers []TeacherTrust `json:"teachers"`
	Current  HeldoutMetrics `json:"current"`
	Shadow   HeldoutMetrics `json:"shadow"`
	Gate     RetrainGate    `json:"gate"`
	// Promoted: la versión nueva se sirve ya. Pending: pasó la puerta pero es
	// microvm y falta su dorado (Snapshot) y /v1/admin/promote.
	Promoted       bool    `json:"promoted"`
	Pending        bool    `json:"pending,omitempty"`
	Version        string  `json:"version,omitempty"`
	Written        string  `json:"written,omitempty"`
	Backup         string  `json:"backup,omitempty"`
	Candidate      string  `json:"candidate,omitempty"` // el .chispa sombra en disco
	Snapshot       string  `json:"snapshot,omitempty"`
	CandidateModel []byte  `json:"candidate_model,omitempty"`
	Cascade        string  `json:"cascade,omitempty"`
	TrainSeconds   float64 `json:"train_seconds"`
	Stored         string  `json:"stored,omitempty"`
	Note           string  `json:"note,omitempty"`
}

// RetrainSummary es un intento, para el historial y /v1/tasks.
type RetrainSummary struct {
	At       time.Time      `json:"at"`
	Current  HeldoutMetrics `json:"current"`
	Shadow   HeldoutMetrics `json:"shadow"`
	Pass     bool           `json:"pass"`
	Promoted string         `json:"promoted,omitempty"`
	Verdict  string         `json:"verdict"`
	Data     RetrainData    `json:"data"`
	Forced   []string       `json:"forced_teachers,omitempty"`
}

// VersionEntry es una versión de Chispa de una tarea.
type VersionEntry struct {
	N        int             `json:"n"`
	SHA256   string          `json:"sha256"`
	File     string          `json:"file"` // relativo a ai-data/<tarea>
	Snapshot string          `json:"snapshot,omitempty"`
	At       time.Time       `json:"at"`
	Source   string          `json:"source"` // initial | retrain | external
	Heldout  *HeldoutMetrics `json:"heldout,omitempty"`
	Data     *RetrainData    `json:"data,omitempty"`
}

// versionsFile es ai-data/<tarea>/versions.json.
type versionsFile struct {
	Task     string           `json:"task"`
	Model    string           `json:"model"`
	Current  int              `json:"current"`
	Previous int              `json:"previous,omitempty"`
	Versions []VersionEntry   `json:"versions"`
	Pending  *VersionEntry    `json:"pending,omitempty"`
	Attempts []RetrainSummary `json:"attempts,omitempty"`
}

const (
	maxAttempts     = 50
	maxVersionFiles = 10
)

func vLabel(n int) string {
	if n <= 0 {
		return ""
	}
	return "v" + strconv.Itoa(n)
}

func (v *versionsFile) entry(n int) *VersionEntry {
	for i := range v.Versions {
		if v.Versions[i].N == n {
			return &v.Versions[i]
		}
	}
	return nil
}

func (v *versionsFile) bySHA(sum string) *VersionEntry {
	for i := len(v.Versions) - 1; i >= 0; i-- {
		if v.Versions[i].SHA256 == sum {
			return &v.Versions[i]
		}
	}
	return nil
}

func (v *versionsFile) last() int {
	n := 0
	for _, e := range v.Versions {
		n = max(n, e.N)
	}
	if v.Pending != nil {
		n = max(n, v.Pending.N)
	}
	return n
}

func (l *learner) manifestPath(task string) string {
	return filepath.Join(l.taskDir(task), "versions.json")
}

// loadManifest lee versions.json (nil si no hay) y lo deja en la caché de
// /metrics y /v1/tasks.
func (l *learner) loadManifest(task string) (*versionsFile, error) {
	if l.dir == "" {
		return nil, nil
	}
	b, err := os.ReadFile(l.manifestPath(task))
	if errors.Is(err, os.ErrNotExist) {
		l.mu.Lock()
		delete(l.manifests, task)
		l.mu.Unlock()
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var v versionsFile
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", l.manifestPath(task), err)
	}
	l.cacheManifest(task, b)
	return &v, nil
}

// cacheManifest guarda una copia PROPIA (decodificada otra vez) para /metrics
// y /v1/tasks: quien reentrena modifica la suya sin candado.
func (l *learner) cacheManifest(task string, b []byte) {
	var c versionsFile
	if json.Unmarshal(b, &c) != nil {
		return
	}
	l.mu.Lock()
	l.manifests[task] = &c
	l.mu.Unlock()
}

func (l *learner) saveManifest(v *versionsFile) error {
	if len(v.Attempts) > maxAttempts {
		v.Attempts = v.Attempts[len(v.Attempts)-maxAttempts:]
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(l.manifestPath(v.Task), append(b, '\n'), 0o600); err != nil {
		return err
	}
	l.cacheManifest(v.Task, b)
	return nil
}

// writeFileAtomic escribe en un temporal del mismo directorio y lo renombra:
// quien lea la ruta ve el fichero viejo o el nuevo, nunca medio.
func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, perm)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// learnModel es el modelo Chispa que aprende una tarea.
func learnModel(tc *TaskConfig) string {
	if tc.Domotica != nil {
		return tc.Domotica.Intent
	}
	return tc.Chispa
}

// ---- el registro que se sirve: versiones y estado de captura

// learnView calcula, para cada tarea con "learn", qué versión sirve y, para
// las microvm con versiones, el dorado al que apuntan (versions.json manda
// sobre el "snapshot" del registro). Lee ficheros: se llama sin candados.
// Devuelve el registro efectivo (una copia si algo cambia).
func (g *Gateway) learnView(c *Config) (*Config, map[string]learnState) {
	states := map[string]learnState{}
	over := map[string]string{}
	for _, task := range sortedKeys(c.Tasks) {
		tc := c.Tasks[task]
		if tc == nil || tc.Learn == nil {
			continue
		}
		st := learnState{cfg: tc.Learn.withDefaults()}
		mc := c.Models[learnModel(tc)]
		if mc == nil {
			continue
		}
		man, err := g.learn.loadManifest(task)
		if err != nil {
			log.Printf("task %s: %v", task, err)
		}
		if mc.Backend == BackendMicroVM {
			if man != nil {
				if e := man.entry(man.Current); e != nil && e.Snapshot != "" {
					over[learnModel(tc)] = e.Snapshot
					st.version, st.sha = vLabel(e.N), e.SHA256
				}
			}
		} else if sum, err := fileSHA256(mc.Path); err == nil {
			st.sha = sum
			if man != nil {
				if e := man.bySHA(sum); e != nil {
					st.version = vLabel(e.N)
				}
			}
		}
		states[task] = st
	}
	if len(over) == 0 {
		return c, states
	}
	eff := *c
	eff.Models = make(map[string]*ModelConfig, len(c.Models))
	for n, m := range c.Models {
		if s, ok := over[n]; ok {
			cp := *m
			cp.Snapshot = s
			m = &cp
		}
		eff.Models[n] = m
	}
	return &eff, states
}

func (g *Gateway) rawConfig() *Config {
	g.cfgMu.RLock()
	defer g.cfgMu.RUnlock()
	return g.rawCfg
}

// refreshLearn vuelve a instalar el registro tal cual (tras promocionar o
// volver atrás) para recalcular versiones, dorados y cascadas.
func (g *Gateway) refreshLearn() []string { return g.setConfig(g.rawConfig()) }

// ---- carga común de review y retrain

type learnTask struct {
	task      string
	tc        *TaskConfig
	lc        LearnConfig
	modelName string
	mc        *ModelConfig
	model     *chispa.Model // el que se sirve (nil: microvm sin copia local)
	modelSHA  string
	raw       []byte // bytes del modelo microvm (su sha256 es el del despliegue)
	labels    map[string]bool
	never     map[string]bool // clases con τ = never
	cases     []*learnCase
	trust     []TeacherTrust
	validated map[string]bool
	man       *versionsFile
}

// loadLearnTask lee todo lo de una tarea: capturas, feedback, el modelo que se
// sirve y la validación de sus maestros. current es el .chispa de una tarea
// microvm sin versiones (puede ir vacío: entonces solo hay etiquetas).
func (g *Gateway) loadLearnTask(ctx context.Context, task string, trust []string, current []byte) (*learnTask, error) {
	cfg := g.config()
	tc := cfg.Tasks[task]
	if tc == nil {
		return nil, statusf(http.StatusNotFound, "unknown task %q", task)
	}
	if tc.Learn == nil {
		return nil, statusf(http.StatusBadRequest, "task %q has no \"learn\" block (docs/mejora-continua.md)", task)
	}
	if g.learn.dir == "" {
		return nil, statusf(http.StatusServiceUnavailable, "this gateway has no data directory (it runs without a registry file)")
	}
	for _, t := range trust {
		if !teacherRE.MatchString(t) {
			return nil, statusf(http.StatusBadRequest, "invalid teacher name %q", t)
		}
	}
	lt := &learnTask{task: task, tc: tc, lc: tc.Learn.withDefaults(), modelName: learnModel(tc), never: map[string]bool{}, labels: map[string]bool{}}
	lt.mc = cfg.Models[lt.modelName]
	man, err := g.learn.loadManifest(task)
	if err != nil {
		return nil, err
	}
	lt.man = man
	if lt.mc.Backend == BackendMicroVM {
		var b []byte
		if man != nil {
			if e := man.entry(man.Current); e != nil {
				if b, err = os.ReadFile(filepath.Join(g.learn.taskDir(task), e.File)); err != nil {
					return nil, fmt.Errorf("version %s of task %s: %w", vLabel(e.N), task, err)
				}
			}
		}
		if b == nil && len(current) > 0 {
			rec, err := g.deploy.chispaLabels(ctx, lt.mc.Snapshot)
			if err != nil {
				return nil, statusf(http.StatusServiceUnavailable, "reading the deploy record of %q: %v", lt.mc.Snapshot, err)
			}
			if sha256Hex(current) != rec.Sha256 {
				return nil, statusf(http.StatusBadRequest, "the given current model (sha256 %.12s) is not the one deployed in %q (%.12s)", sha256Hex(current), lt.mc.Snapshot, rec.Sha256)
			}
			b = current
		}
		if b != nil {
			if lt.model, err = chispa.Unmarshal(b); err != nil {
				return nil, err
			}
			lt.modelSHA, lt.raw = sha256Hex(b), b
		} else if rec, err := g.deploy.chispaLabels(ctx, lt.mc.Snapshot); err == nil {
			for _, l := range rec.Labels {
				lt.labels[l] = true
			}
		}
	} else {
		if lt.model, err = g.chispa.get(lt.mc.Path); err != nil {
			return nil, statusf(http.StatusServiceUnavailable, "chispa model %q unavailable: %v", lt.modelName, err)
		}
		if lt.modelSHA, err = fileSHA256(lt.mc.Path); err != nil {
			return nil, err
		}
	}
	var predict func(string, map[string]any) (CaptureChispa, bool)
	if m := lt.model; m != nil {
		for i, l := range m.Labels {
			lt.labels[l] = true
			if m.Thresholds[i] >= chispa.NeverConfident {
				lt.never[l] = true
			}
		}
		predict = func(text string, fields map[string]any) (CaptureChispa, bool) {
			p := m.PredictFull(chispa.Input{Text: text, Fields: fields}, 0)
			return CaptureChispa{Label: p.Label, Prob: round4(p.Prob), Top: roundTop(topN(p.Probs, learnTopN))}, true
		}
	}
	g.learn.flush()
	caps, err := g.learn.readCaptures(task, int64(lt.lc.MaxMB)<<20+chispa.MaxLineBytes)
	if err != nil {
		return nil, err
	}
	fb, err := g.learn.readFeedback(task)
	if err != nil {
		return nil, err
	}
	lt.cases = buildCases(caps, fb, predict)
	lt.trust = validateTeachers(lt.cases, lt.lc, lt.labels, g.evalTeachers(task, tc), append(append([]string(nil), lt.lc.TrustTeachers...), trust...))
	lt.validated = map[string]bool{}
	for _, t := range lt.trust {
		if t.Validated {
			lt.validated[t.Name] = true
		}
	}
	return lt, nil
}

// evalTeachers son los maestros que el registro de evaluación de la tarea
// respalda contra etiquetas de verdad: el VON de una cascada que gana, o el
// codificador de una capa 3 que gana.
func (g *Gateway) evalTeachers(task string, tc *TaskConfig) map[string]string {
	out := map[string]string{}
	p := g.evalPath(task)
	if p == "" {
		return out
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return out
	}
	if tc.Domotica != nil {
		var r DomoticaEvalRecord
		if json.Unmarshal(b, &r) == nil && r.Kind == "domotica" && r.BeatsFast {
			out[r.Encoder.Model] = r.Verdict
		}
		return out
	}
	var r EvalRecord
	if json.Unmarshal(b, &r) == nil && r.BeatsChispa {
		out[r.VON.Model] = r.Verdict
	}
	return out
}

// ---- medir en el conjunto de confianza

func heldoutEval(m *chispa.Model, tc *TaskConfig, exs []chispa.Example) (HeldoutMetrics, []bool) {
	var h HeldoutMetrics
	h.Examples = len(exs)
	answered := make([]bool, len(exs))
	var right, conf, confRight int
	for i, ex := range exs {
		p, _, ok := chispaDecide(m, tc, ex.Input(), false)
		r := p.Label == ex.Label
		if r {
			right++
		}
		if ok {
			conf++
			if r {
				confRight++
				answered[i] = true
			} else {
				h.ConfidentErrors++
			}
		}
	}
	n := len(exs)
	h.Accuracy, h.Coverage = ratio(right, n), ratio(conf, n)
	h.ConfidentPrecision, h.AnsweredRight = ratio(confRight, conf), ratio(confRight, n)
	return h, answered
}

// decideGate aplica la puerta a dos medidas sobre los mismos ejemplos.
func decideGate(cur, sh HeldoutMetrics, ca, sa []bool, rule string, tol float64) RetrainGate {
	g := RetrainGate{Rule: rule, PrecisionTolerance: tol}
	for i := range ca {
		switch {
		case sa[i] && !ca[i]:
			g.ShadowOnlyRight++
		case ca[i] && !sa[i]:
			g.CurrentOnlyRight++
		}
	}
	g.PValue = mcnemarOneSided(g.CurrentOnlyRight, g.ShadowOnlyRight)
	g.Significant = g.ShadowOnlyRight > g.CurrentOnlyRight && g.PValue < 0.05
	g.NoRegression = sh.AnsweredRight >= cur.AnsweredRight && sh.Accuracy >= cur.Accuracy
	// Sin respuestas confiadas no se promete nada: una sombra que no contesta
	// nunca no "mantiene la precisión".
	g.PrecisionOK = sh.Coverage > 0 && sh.ConfidentPrecision+1e-9 >= cur.ConfidentPrecision-tol
	g.Pass = g.PrecisionOK && (g.Significant || (rule == "no-regression" && g.NoRegression))
	detail := fmt.Sprintf("answered right %.4f vs %.4f (+%d/-%d, McNemar p=%.2g), confident precision %.4f vs %.4f, coverage %.4f vs %.4f, accuracy %.4f vs %.4f on %d held-out examples",
		sh.AnsweredRight, cur.AnsweredRight, g.ShadowOnlyRight, g.CurrentOnlyRight, g.PValue,
		sh.ConfidentPrecision, cur.ConfidentPrecision, sh.Coverage, cur.Coverage, sh.Accuracy, cur.Accuracy, cur.Examples)
	switch {
	case g.Pass && g.Significant:
		g.Verdict = "the new model wins: " + detail
	case g.Pass:
		g.Verdict = "the new model does not regress (rule no-regression): " + detail
	case !g.PrecisionOK:
		g.Verdict = fmt.Sprintf("rejected: confident precision would drop more than %.3f: %s", tol, detail)
	case g.CurrentOnlyRight >= g.ShadowOnlyRight:
		g.Verdict = "rejected: the current model is as good or better: " + detail
	default:
		g.Verdict = "rejected: not enough evidence the new model is better: " + detail
	}
	return g
}

// normText es la clave de la guarda de fugas: el mismo texto con otras
// mayúsculas o espacios sigue siendo el mismo ejemplo.
func normText(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }

func readLabelled(path string) ([]chispa.Example, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	exs, err := chispa.ReadExamples(f, 0, true)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return exs, nil
}

// trainConfigFrom son los hiperparámetros del modelo que se sirve: la sombra
// solo cambia en los datos, así que la puerta mide los datos.
func trainConfigFrom(m *chispa.Model, target float64, seed uint64) train.Config {
	c := train.Config{Spec: m.Spec, TargetPrecision: target, OneVsRest: m.Meta.OneVsRest, Seed: seed}
	p := m.Meta.Params
	f := func(k string) float64 { v, _ := strconv.ParseFloat(p[k], 64); return v }
	i := func(k string) int { v, _ := strconv.Atoi(p[k]); return v }
	c.LearningRate, c.L2, c.MaxClassWeight = f("learning_rate"), f("l2"), f("max_class_weight")
	if c.L2 == 0 && p["l2"] == "0" {
		c.L2 = -1 // 0 en el modelo = sin L2; en Config, 0 = por defecto
	}
	c.ClassWeight, c.MaxEpochs, c.Patience, c.MinSupport = p["class_weight"], i("max_epochs"), i("patience"), i("min_support")
	if c.Seed == 0 {
		s, _ := strconv.ParseUint(p["seed"], 10, 64)
		c.Seed = s
	}
	return c
}

// Retrain entrena una sombra, la mide contra la que se sirve en el conjunto de
// confianza y la promociona solo si pasa la puerta.
func (g *Gateway) Retrain(ctx context.Context, req RetrainRequest) (*RetrainReport, error) {
	g.calMu.Lock()
	defer g.calMu.Unlock()
	switch req.Rule {
	case "":
		req.Rule = "mcnemar"
	case "mcnemar", "no-regression":
	default:
		return nil, statusf(http.StatusBadRequest, "rule must be mcnemar or no-regression")
	}
	tol := 0.005
	if req.PrecisionTolerance != nil {
		tol = *req.PrecisionTolerance
	}
	if !(tol >= 0 && tol <= 0.1) {
		return nil, statusf(http.StatusBadRequest, "precision_tolerance must be in [0,0.1]")
	}
	lt, err := g.loadLearnTask(ctx, req.Task, req.Trust, req.CurrentModel)
	if err != nil {
		return nil, err
	}
	cfg := g.config()
	for _, n := range sortedKeys(cfg.Tasks) {
		if t := cfg.Tasks[n]; n != req.Task && learnModel(t) == lt.modelName {
			return nil, statusf(http.StatusBadRequest, "chispa model %q is also used by task %q: retraining it for %q would change both; give the task its own model", lt.modelName, n, req.Task)
		}
	}
	if lt.model == nil {
		return nil, statusf(http.StatusBadRequest, "task %q (backend microvm) has no local copy of the model it serves yet: pass it once (kling ai retrain %s -current <deployed.chispa>)", req.Task, req.Task)
	}
	if lt.lc.Gold == "" || lt.lc.Heldout == "" {
		return nil, statusf(http.StatusBadRequest, "task %q: learn needs \"gold\" (the training set, always kept) and \"heldout\" (trusted labels no training sees) to retrain", req.Task)
	}
	gold, err := readLabelled(lt.lc.Gold)
	if err != nil {
		return nil, statusf(http.StatusBadRequest, "gold: %v", err)
	}
	held, err := readLabelled(lt.lc.Heldout)
	if err != nil {
		return nil, statusf(http.StatusBadRequest, "heldout: %v", err)
	}
	var valid []chispa.Example
	if lt.lc.Valid != "" {
		if valid, err = readLabelled(lt.lc.Valid); err != nil {
			return nil, statusf(http.StatusBadRequest, "valid: %v", err)
		}
	}
	goldCount := map[string]int{}
	for _, ex := range gold {
		goldCount[ex.Label]++
	}
	sel := selectCases(lt.cases, lt.lc, lt.labels, lt.validated, goldCount)

	rep := &RetrainReport{Task: req.Task, Model: lt.modelName, At: time.Now().UTC().Truncate(time.Second), Teachers: lt.trust, Reasons: sel.Reasons}
	rep.Data.Pending, rep.Data.Discarded, rep.Data.Capped = sel.Pending, sel.Discarded, sel.Capped

	// Guarda de fugas: nada del conjunto de confianza entra a entrenar ni a
	// calibrar. Si entrara, la sombra parecería mejor de lo que es.
	heldKeys := make(map[string]bool, len(held))
	for _, ex := range held {
		heldKeys[normText(ex.Text)] = true
	}
	clean := func(exs []chispa.Example) []chispa.Example {
		out := exs[:0:0]
		for _, ex := range exs {
			if heldKeys[normText(ex.Text)] {
				rep.Data.Leaked++
				continue
			}
			out = append(out, ex)
		}
		return out
	}
	gold, valid = clean(gold), clean(valid)
	human, accepted := clean(sel.Human), clean(sel.Accepted)
	rep.Data.Gold, rep.Data.Human, rep.Data.Accepted = len(gold), len(human), len(accepted)

	target := lt.tc.Precision
	if target == 0 {
		target = lt.model.Meta.TargetPrecision
	}
	cur, curAns := heldoutEval(lt.model, lt.tc, held)
	cur.SHA256 = lt.modelSHA
	if lt.man != nil {
		if e := lt.man.bySHA(lt.modelSHA); e != nil {
			cur.Version = vLabel(e.N)
		}
	}
	rep.Current = cur
	if len(human)+len(accepted) == 0 {
		rep.Gate = RetrainGate{Rule: req.Rule, PrecisionTolerance: tol,
			Verdict: "nothing new to learn from: no human labels and nothing accepted from validated teachers (see kling ai review)"}
		g.rememberAttempt(lt, rep)
		return rep, nil
	}

	// Validación: la del fichero, o un 10 % del oro (como `kling chispa
	// train` sin -valid); y un 20 % estable de lo humano, para que la
	// calibración vea también tráfico de verdad. Lo de los maestros nunca
	// calibra: no es verdad.
	trainEx := append([]chispa.Example(nil), gold...)
	if len(valid) == 0 {
		cfgSeed := trainConfigFrom(lt.model, target, req.Seed).Seed
		if cfgSeed == 0 {
			cfgSeed = 1
		}
		trainEx, valid = train.SplitValidation(gold, 0.1, cfgSeed)
	}
	for _, ex := range human {
		if chispa.FNV1a64(normText(ex.Text))%5 == 0 {
			valid = append(valid, ex)
		} else {
			trainEx = append(trainEx, ex)
		}
	}
	trainEx = append(trainEx, accepted...)
	rep.Data.Train, rep.Data.Valid = len(trainEx), len(valid)

	tcfg := trainConfigFrom(lt.model, target, req.Seed)
	tcfg.CreatedAt = rep.At.Format(time.RFC3339)
	dh := sha256.New()
	enc := json.NewEncoder(dh)
	for i := range trainEx {
		_ = enc.Encode(trainEx[i])
	}
	tcfg.DatasetSHA256 = hex.EncodeToString(dh.Sum(nil))
	t0 := time.Now()
	res, err := train.Train(trainEx, valid, tcfg)
	if err != nil {
		return nil, fmt.Errorf("training the shadow model: %w", err)
	}
	rep.TrainSeconds = math.Round(time.Since(t0).Seconds()*100) / 100
	res.Model.Meta.Notes = fmt.Sprintf("%s[kling ai retrain %s: gold %d, human %d, accepted %d (weight %.2f)] ",
		lt.model.Meta.Notes, rep.At.Format(time.RFC3339), len(gold), len(human), len(accepted), lt.lc.TeacherWeight)
	nb, err := res.Model.Marshal()
	if err != nil {
		return nil, err
	}
	shadow, err := chispa.Unmarshal(nb) // se mide y se sirve lo que se leería del disco
	if err != nil {
		return nil, err
	}
	sh, shAns := heldoutEval(shadow, lt.tc, held)
	sh.SHA256 = sha256Hex(nb)
	rep.Shadow = sh
	rep.Gate = decideGate(cur, sh, curAns, shAns, req.Rule, tol)

	vdir := filepath.Join(g.learn.taskDir(req.Task), "versions")
	shadowPath := filepath.Join(vdir, lt.modelName+"@shadow.chispa")
	if err := writeFileAtomic(shadowPath, nb, 0o600); err != nil {
		return nil, err
	}
	rep.Candidate = shadowPath
	if !rep.Gate.Pass || req.DryRun {
		if req.DryRun && rep.Gate.Pass {
			rep.Note = "dry run: the new model would be promoted; nothing changed"
		}
		g.rememberAttempt(lt, rep)
		g.saveRetrainRecord(rep)
		return rep, nil
	}
	if err := g.promote(ctx, lt, rep, nb, req); err != nil {
		return nil, err
	}
	g.rememberAttempt(lt, rep)
	g.saveRetrainRecord(rep)
	return rep, nil
}

// ensureManifest crea versions.json con la versión que se sirve hoy como v1
// (o la añade como "external" si cambió por fuera de retrain), para que
// siempre haya a qué volver.
func (g *Gateway) ensureManifest(lt *learnTask, cur HeldoutMetrics) (*versionsFile, error) {
	man := lt.man
	if man == nil {
		man = &versionsFile{Task: lt.task, Model: lt.modelName}
	}
	if e := man.bySHA(lt.modelSHA); e != nil {
		if e.Heldout == nil {
			h := cur
			h.Version = vLabel(e.N)
			e.Heldout = &h
		}
		if man.Current != e.N && lt.mc.Backend != BackendMicroVM {
			man.Current = e.N
		}
		return man, nil
	}
	// Lo que se guarda es el fichero que se sirve, no una re-serialización:
	// su sha256 tiene que ser el mismo que miran las evaluaciones y el
	// registro de despliegue.
	b := lt.raw
	if lt.mc.Backend != BackendMicroVM {
		var err error
		if b, err = os.ReadFile(lt.mc.Path); err != nil {
			return nil, err
		}
	}
	n := man.last() + 1
	src := "initial"
	if n > 1 {
		src = "external"
	}
	e := VersionEntry{N: n, SHA256: sha256Hex(b), File: filepath.Join("versions", lt.modelName+"@"+vLabel(n)+".chispa"), At: time.Now().UTC().Truncate(time.Second), Source: src}
	if lt.mc.Backend == BackendMicroVM {
		e.Snapshot = lt.mc.Snapshot
	}
	h := cur
	h.Version = vLabel(n)
	e.Heldout = &h
	if err := writeFileAtomic(filepath.Join(g.learn.taskDir(lt.task), e.File), b, 0o600); err != nil {
		return nil, err
	}
	man.Versions = append(man.Versions, e)
	man.Current = n
	return man, nil
}

// promote deja la sombra como versión nueva: servida ya (en proceso) o
// pendiente de su dorado (microvm).
func (g *Gateway) promote(ctx context.Context, lt *learnTask, rep *RetrainReport, nb []byte, req RetrainRequest) error {
	man, err := g.ensureManifest(lt, rep.Current)
	if err != nil {
		return err
	}
	rep.Current.Version = vLabel(man.Current)
	n := man.last() + 1
	data := rep.Data
	sh := rep.Shadow
	sh.Version = vLabel(n)
	rep.Shadow.Version = sh.Version
	e := VersionEntry{N: n, SHA256: sha256Hex(nb), File: filepath.Join("versions", lt.modelName+"@"+vLabel(n)+".chispa"),
		At: rep.At, Source: "retrain", Heldout: &sh, Data: &data}
	if err := writeFileAtomic(filepath.Join(g.learn.taskDir(lt.task), e.File), nb, 0o600); err != nil {
		return err
	}
	rep.Version = vLabel(n)
	if lt.mc.Backend == BackendMicroVM {
		base := g.rawConfig().Models[lt.modelName].Snapshot
		e.Snapshot = fmt.Sprintf("%s-v%d", base, n)
		man.Pending = &e
		if err := g.learn.saveManifest(man); err != nil {
			return err
		}
		rep.Pending, rep.Snapshot, rep.CandidateModel = true, e.Snapshot, nb
		rep.Note = fmt.Sprintf("passed the gate; backend microvm: deploy it as %s (kling chispa deploy %s -model <file>) and confirm with /v1/admin/promote; `kling ai retrain` does both", e.Snapshot, e.Snapshot)
		return nil
	}
	before := g.cascade(lt.task)
	path := lt.mc.Path
	old, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path+".prev", old, 0o644); err != nil {
		return fmt.Errorf("saving backup: %w", err)
	}
	nm, err := chispa.Unmarshal(nb)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, nb, 0o644); err != nil {
		return err
	}
	g.chispa.put(path, nm)
	man.Previous, man.Current = man.Current, n
	man.Versions = append(man.Versions, e)
	pruneVersions(g.learn.taskDir(lt.task), man)
	if err := g.learn.saveManifest(man); err != nil {
		return err
	}
	rep.Promoted, rep.Written, rep.Backup = true, path, path+".prev"
	log.Printf("task %s: promoted %s %s (%s)", lt.task, lt.modelName, rep.Version, rep.Gate.Verdict)
	g.refreshLearn()
	rep.Cascade = g.reEvaluate(ctx, lt, before, req)
	return nil
}

// pruneVersions borra los ficheros de versiones viejas (se quedan la v1, la
// que se sirve, la anterior y las últimas): el disco no crece con cada
// reentreno. La entrada sigue en versions.json, sin fichero.
func pruneVersions(dir string, man *versionsFile) {
	keep := map[int]bool{1: true, man.Current: true, man.Previous: true}
	ns := make([]int, 0, len(man.Versions))
	for _, e := range man.Versions {
		ns = append(ns, e.N)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ns)))
	for i, n := range ns {
		if i < maxVersionFiles {
			keep[n] = true
		}
	}
	for i := range man.Versions {
		e := &man.Versions[i]
		if !keep[e.N] && e.File != "" && e.Snapshot == "" {
			_ = os.Remove(filepath.Join(dir, e.File))
			e.File = ""
		}
	}
}

// reEvaluate vuelve a decidir la cascada (o la capa del codificador) tras un
// modelo nuevo: su registro de evaluación va atado al sha256 del .chispa.
func (g *Gateway) reEvaluate(ctx context.Context, lt *learnTask, before CascadeState, req RetrainRequest) string {
	after := g.cascade(lt.task)
	if !before.On() || after.On() {
		return ""
	}
	off := func(why string) string {
		return fmt.Sprintf("the %s is off until it is evaluated again (its eval was for the previous model): %s", layerName(lt.tc), why)
	}
	if req.NoEval {
		return off("run kling ai eval " + lt.task + " -data <held-out>")
	}
	if lt.tc.Domotica != nil {
		if len(req.EvalRows) == 0 {
			return off("a domotica task needs rows: kling ai retrain -eval rows.jsonl, or kling ai eval " + lt.task + " -data rows.jsonl")
		}
		rec, err := g.evalDomotica(ctx, EvalRequest{Task: lt.task, Data: "after retrain", Rows: req.EvalRows})
		if err != nil {
			return off(err.Error())
		}
		return fmt.Sprintf("re-evaluated the encoder layer: %s; now %s", rec.Verdict, g.cascade(lt.task).Status)
	}
	exs := req.EvalExamples
	if len(exs) == 0 {
		held, err := readLabelled(lt.lc.Heldout)
		if err != nil {
			return off(err.Error())
		}
		for _, ex := range held {
			exs = append(exs, EvalExample{Text: ex.Text, Fields: ex.Fields, Label: ex.Label})
		}
	}
	rec, err := g.Eval(ctx, EvalRequest{Task: lt.task, Data: "learn.heldout (after retrain)", Examples: exs})
	if err != nil {
		return off(err.Error())
	}
	return fmt.Sprintf("re-evaluated the cascade: %s; now %s", rec.Verdict, g.cascade(lt.task).Status)
}

func layerName(tc *TaskConfig) string {
	if tc.Domotica != nil {
		return "encoder layer"
	}
	return "cascade"
}

func (g *Gateway) rememberAttempt(lt *learnTask, rep *RetrainReport) {
	s := &RetrainSummary{At: rep.At, Current: rep.Current, Shadow: rep.Shadow, Pass: rep.Gate.Pass, Verdict: rep.Gate.Verdict, Data: rep.Data}
	if rep.Promoted || rep.Pending {
		s.Promoted = rep.Version
	}
	for _, t := range rep.Teachers {
		if t.Source == "forced" {
			s.Forced = append(s.Forced, t.Name)
		}
	}
	g.learn.mu.Lock()
	g.learn.lastRun[lt.task] = s
	g.learn.pending[lt.task] = rep.Data.Pending
	g.learn.mu.Unlock()
	if rep.Shadow.Examples == 0 {
		return // no hubo sombra: nada que apuntar en el historial
	}
	man, err := g.learn.loadManifest(lt.task)
	if err != nil {
		return
	}
	if man == nil {
		man = &versionsFile{Task: lt.task, Model: lt.modelName}
	}
	man.Attempts = append(man.Attempts, *s)
	if err := g.learn.saveManifest(man); err != nil {
		log.Printf("task %s: saving versions: %v", lt.task, err)
	}
}

// saveRetrainRecord guarda el último informe en ai-evals/<tarea>-retrain.json.
func (g *Gateway) saveRetrainRecord(rep *RetrainReport) {
	if g.opts.EvalDir == "" {
		return
	}
	cp := *rep
	cp.CandidateModel = nil
	p := filepath.Join(g.opts.EvalDir, rep.Task+"-retrain.json")
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return
	}
	if err := writeFileAtomic(p, append(b, '\n'), 0o644); err != nil {
		log.Printf("task %s: saving retrain record: %v", rep.Task, err)
		return
	}
	rep.Stored = p
}

// ---- /v1/admin/promote y /v1/admin/rollback

// PromoteRequest confirma una versión pendiente de una tarea microvm cuando
// su dorado ya existe.
type PromoteRequest struct {
	Task     string `json:"task"`
	Version  string `json:"version"` // "v3"
	Snapshot string `json:"snapshot"`
}

// VersionChange es la respuesta de promote y rollback.
type VersionChange struct {
	Task     string   `json:"task"`
	From     string   `json:"from,omitempty"`
	To       string   `json:"to"`
	Snapshot string   `json:"snapshot,omitempty"`
	Written  string   `json:"written,omitempty"`
	Cascades []string `json:"cascades,omitempty"`
}

func parseVersion(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(s, "v"))
	return n, err == nil && n > 0
}

// Promote termina la promoción de una versión microvm.
func (g *Gateway) Promote(ctx context.Context, req PromoteRequest) (*VersionChange, error) {
	g.calMu.Lock()
	defer g.calMu.Unlock()
	man, _, err := g.learnManifest(req.Task)
	if err != nil {
		return nil, err
	}
	n, ok := parseVersion(req.Version)
	if !ok || man.Pending == nil || man.Pending.N != n {
		return nil, statusf(http.StatusConflict, "task %q has no pending version %q (retrain again)", req.Task, req.Version)
	}
	if req.Snapshot == "" {
		req.Snapshot = man.Pending.Snapshot
	}
	rec, err := g.deploy.chispaLabels(ctx, req.Snapshot)
	if err != nil {
		return nil, statusf(http.StatusServiceUnavailable, "reading the deploy record of %q: %v", req.Snapshot, err)
	}
	if rec.Sha256 != man.Pending.SHA256 {
		return nil, statusf(http.StatusConflict, "snapshot %q serves a model with sha256 %.12s, not version %s (%.12s)", req.Snapshot, rec.Sha256, req.Version, man.Pending.SHA256)
	}
	e := *man.Pending
	e.Snapshot = req.Snapshot
	from := vLabel(man.Current)
	man.Versions = append(man.Versions, e)
	man.Previous, man.Current, man.Pending = man.Current, n, nil
	if err := g.learn.saveManifest(man); err != nil {
		return nil, err
	}
	g.deployMu.Lock()
	delete(g.deployCache, req.Snapshot)
	g.deployMu.Unlock()
	return &VersionChange{Task: req.Task, From: from, To: vLabel(n), Snapshot: req.Snapshot, Cascades: g.refreshLearn()}, nil
}

// RollbackRequest vuelve a una versión anterior (por defecto, la de antes).
type RollbackRequest struct {
	Task string `json:"task"`
	To   string `json:"to,omitempty"`
}

func (g *Gateway) learnManifest(task string) (*versionsFile, *TaskConfig, error) {
	cfg := g.config()
	tc := cfg.Tasks[task]
	if tc == nil {
		return nil, nil, statusf(http.StatusNotFound, "unknown task %q", task)
	}
	if tc.Learn == nil || g.learn.dir == "" {
		return nil, nil, statusf(http.StatusBadRequest, "task %q has no \"learn\" block (or the gateway has no data directory)", task)
	}
	man, err := g.learn.loadManifest(task)
	if err != nil {
		return nil, nil, err
	}
	if man == nil || len(man.Versions) == 0 {
		return nil, nil, statusf(http.StatusConflict, "task %q has no versions yet (kling ai retrain creates them)", task)
	}
	return man, tc, nil
}

// Rollback vuelve a servir una versión anterior, al instante: en proceso,
// el fichero de esa versión pasa a la ruta del registro (con un rename); en
// microvm, la tarea vuelve a apuntar a su dorado, que sigue ahí.
func (g *Gateway) Rollback(req RollbackRequest) (*VersionChange, error) {
	g.calMu.Lock()
	defer g.calMu.Unlock()
	man, tc, err := g.learnManifest(req.Task)
	if err != nil {
		return nil, err
	}
	target := man.Previous
	if req.To != "" {
		n, ok := parseVersion(req.To)
		if !ok {
			return nil, statusf(http.StatusBadRequest, "version must look like v2")
		}
		target = n
	}
	e := man.entry(target)
	if target == 0 || e == nil {
		return nil, statusf(http.StatusConflict, "task %q: no version to go back to (have %s)", req.Task, versionList(man))
	}
	if target == man.Current {
		return nil, statusf(http.StatusConflict, "task %q already serves %s", req.Task, vLabel(target))
	}
	mc := g.rawConfig().Models[learnModel(tc)]
	out := &VersionChange{Task: req.Task, From: vLabel(man.Current), To: vLabel(target)}
	if mc.Backend == BackendMicroVM {
		if e.Snapshot == "" {
			return nil, statusf(http.StatusConflict, "version %s has no golden snapshot", vLabel(target))
		}
		out.Snapshot = e.Snapshot
	} else {
		if e.File == "" {
			return nil, statusf(http.StatusConflict, "the file of version %s was pruned", vLabel(target))
		}
		b, err := os.ReadFile(filepath.Join(g.learn.taskDir(req.Task), e.File))
		if err != nil {
			return nil, err
		}
		if sha256Hex(b) != e.SHA256 {
			return nil, statusf(http.StatusConflict, "the file of version %s does not match its sha256: refusing to serve it", vLabel(target))
		}
		nm, err := chispa.Unmarshal(b)
		if err != nil {
			return nil, err
		}
		if old, err := os.ReadFile(mc.Path); err == nil {
			if err := writeFileAtomic(mc.Path+".prev", old, 0o644); err != nil {
				return nil, err
			}
		}
		if err := writeFileAtomic(mc.Path, b, 0o644); err != nil {
			return nil, err
		}
		g.chispa.put(mc.Path, nm)
		out.Written = mc.Path
	}
	man.Previous, man.Current = man.Current, target
	if err := g.learn.saveManifest(man); err != nil {
		return nil, err
	}
	log.Printf("task %s: rolled back %s -> %s", req.Task, out.From, out.To)
	out.Cascades = g.refreshLearn()
	return out, nil
}

func versionList(man *versionsFile) string {
	var s []string
	for _, e := range man.Versions {
		s = append(s, vLabel(e.N))
	}
	return strings.Join(s, ", ")
}
