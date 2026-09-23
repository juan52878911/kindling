package aigw

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/jev"
)

// RECALIBRACIÓN CON VON COMO MAESTRO.
//
// docs/JEV-EVAL.md midió que los umbrales de JEV prometen una precisión (0,95
// en validación) que el tráfico real no cumple cuando la distribución cambia
// (0,58–0,85). El remedio honrado es recalibrar con tráfico reciente del mismo
// sitio. Aquí no hay etiquetas de verdad, pero hay algo parecido: lo que VON
// contesta en lo escalado. Se guarda en un anillo acotado y `kling ai
// calibrate` reajusta con eso los umbrales por clase.
//
// Dos sesgos que se corrigen, y uno que no:
//
//  1. Solo lo escalado tendría respuesta de VON, así que la muestra no diría
//     nada de si lo que JEV contesta confiado está bien. Por eso existe
//     "audit": una fracción de las respuestas confiadas también se pregunta a
//     VON (en segundo plano, sin retrasar al cliente). Cada muestra lleva un
//     peso = 1/probabilidad de haber entrado en la muestra (1 lo escalado,
//     1/audit lo auditado), y la precisión se estima ponderada.
//  2. Ajustar y medir sobre las mismas muestras promete de más. Se ajusta con
//     la mitad y se decide con la otra mitad.
//  3. VON también se equivoca. La precisión que se mide es CONCORDANCIA con
//     VON, no acierto. Si VON acierta un 60 %, un umbral "al 95 % de
//     concordancia" no da un 95 % de aciertos. Lo que sí da es que JEV solo
//     contesta donde habría dicho lo mismo que el modelo al que escalaría, que
//     es lo que se le pide a una cascada. Por eso nunca es automático.

type sample struct {
	pred    string  // etiqueta de JEV
	prob    float64 // su probabilidad calibrada
	teacher string  // lo que contestó VON (nunca Unknown: esas no se guardan)
	weight  float64 // 1/probabilidad de entrar en la muestra
	at      time.Time
}

// ring es un búfer circular de muestras por tarea.
type ring struct {
	mu   sync.Mutex
	buf  []sample
	next int
	full bool
}

func newRing(n int) *ring { return &ring{buf: make([]sample, n)} }

func (r *ring) add(s sample) {
	r.mu.Lock()
	r.buf[r.next] = s
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

// snapshot copia las muestras en orden de llegada.
func (r *ring) snapshot() []sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return append([]sample(nil), r.buf[:r.next]...)
	}
	return append(append([]sample(nil), r.buf[r.next:]...), r.buf[:r.next]...)
}

func (r *ring) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.full {
		return len(r.buf)
	}
	return r.next
}

// CalibrateRequest es la petición de `kling ai calibrate`.
type CalibrateRequest struct {
	Task       string  `json:"task"`
	Target     float64 `json:"target,omitempty"`      // 0 = la de la tarea o la del modelo
	MinSupport int     `json:"min_support,omitempty"` // 0 = 10, como al entrenar
	DryRun     bool    `json:"dry_run,omitempty"`
}

// ClassCal es lo que pasa con una clase.
type ClassCal struct {
	Label  string  `json:"label"`
	Old    float64 `json:"old_threshold"`
	New    float64 `json:"new_threshold"`
	Tuned  int     `json:"tune_predictions"` // predicciones de la clase en la mitad de ajuste
	Forced bool    `json:"override,omitempty"`
}

// CalEval es la cascada medida en la mitad de evaluación con unos umbrales.
type CalEval struct {
	Coverage  float64 `json:"coverage"`  // fracción (ponderada) que JEV contestaría
	Agreement float64 `json:"agreement"` // de eso, fracción en que coincide con VON
	Confident int     `json:"confident_samples"`
}

// CalibrateReport es la respuesta: lo que se midió y lo que se hizo.
type CalibrateReport struct {
	Task       string     `json:"task"`
	Model      string     `json:"model"`
	Samples    int        `json:"samples"`   // con respuesta de VON válida
	Escalated  int        `json:"escalated"` // de ellas, escaladas
	Audited    int        `json:"audited"`   // de ellas, auditorías
	Target     float64    `json:"target"`    // concordancia objetivo
	Before     CalEval    `json:"before"`    // mitad de evaluación, umbrales actuales
	After      CalEval    `json:"after"`     // mitad de evaluación, umbrales nuevos
	Classes    []ClassCal `json:"classes"`   // umbrales por clase
	Improved   bool       `json:"improved"`  // si los nuevos mejoran la promesa
	Written    string     `json:"written,omitempty"`
	Backup     string     `json:"backup,omitempty"`
	Reason     string     `json:"reason"`
	TeacherErr string     `json:"teacher_warning"` // el límite que no se corrige
}

// chooseWeighted es jev.ChooseThresholds con pesos: el menor corte por clase
// cuya concordancia ponderada, estimada como aciertos/(n+1) igual que al
// entrenar (conservador con poca muestra), llega al objetivo con al menos
// minSupport muestras sin ponderar.
func chooseWeighted(nLabels int, pred, gold []int, prob, w []float64, target float64, minSupport int) ([]float64, []int) {
	out := make([]float64, nLabels)
	counts := make([]int, nLabels)
	for c := 0; c < nLabels; c++ {
		type row struct {
			p, w float64
			ok   bool
		}
		var rows []row
		for i := range pred {
			if pred[i] == c {
				rows = append(rows, row{prob[i], w[i], gold[i] == c})
			}
		}
		counts[c] = len(rows)
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].p > rows[j].p })
		out[c] = jev.NeverConfident
		var tp, n float64
		for i, r := range rows {
			if r.ok {
				tp += r.w
			}
			n += r.w
			if i+1 < len(rows) && rows[i+1].p == r.p {
				continue // los empates entran o salen juntos
			}
			if i+1 >= minSupport && tp/(n+1) >= target {
				out[c] = r.p
			}
		}
	}
	return out, counts
}

// evalCascade mide cobertura y concordancia con unos umbrales.
func evalCascade(pred, gold []int, prob, w, tau []float64) CalEval {
	var tot, conf, ok float64
	var n int
	for i := range pred {
		tot += w[i]
		if prob[i] >= tau[pred[i]] {
			conf += w[i]
			n++
			if pred[i] == gold[i] {
				ok += w[i]
			}
		}
	}
	e := CalEval{Confident: n, Agreement: 1} // sin respuestas confiadas no se promete nada falso
	if tot > 0 {
		e.Coverage = conf / tot
	}
	if conf > 0 {
		e.Agreement = ok / conf
	}
	return e
}

// calibrate calcula los umbrales nuevos de una tarea sobre su muestra. No
// escribe nada: eso lo decide quien llama según Improved y DryRun.
func calibrate(m *jev.Model, samples []sample, overrides map[string]float64, target float64, minSupport int) (*CalibrateReport, []float64) {
	rep := &CalibrateReport{Target: target,
		TeacherErr: "agreement is measured against VON, which can be wrong too: it bounds how often JEV disagrees with the model it would escalate to, not how often it is right"}
	var pred, gold []int
	var prob, w []float64
	for _, s := range samples {
		p, g := m.GoldIndex(s.pred), m.GoldIndex(s.teacher)
		if p < 0 || g < 0 {
			continue // etiquetas de otra versión del modelo
		}
		pred, gold, prob, w = append(pred, p), append(gold, g), append(prob, s.prob), append(w, s.weight)
		if s.weight > 1 {
			rep.Audited++
		} else {
			rep.Escalated++
		}
	}
	rep.Samples = len(pred)

	// Mitades alternas: la muestra está en orden de llegada, así que las dos
	// ven la misma época del tráfico.
	var tp, tg, ep, eg []int
	var tpr, tw, epr, ew []float64
	for i := range pred {
		if i%2 == 0 {
			tp, tg, tpr, tw = append(tp, pred[i]), append(tg, gold[i]), append(tpr, prob[i]), append(tw, w[i])
		} else {
			ep, eg, epr, ew = append(ep, pred[i]), append(eg, gold[i]), append(epr, prob[i]), append(ew, w[i])
		}
	}

	old := effective(m, overrides)
	nuevo, counts := chooseWeighted(len(m.Labels), tp, tg, tpr, tw, target, minSupport)
	// Un umbral forzado en la tarea sigue mandando al servir: se informa, y el
	// .jev guarda el calculado.
	for c, l := range m.Labels {
		_, forced := overrides[l]
		rep.Classes = append(rep.Classes, ClassCal{Label: l, Old: old[c], New: nuevo[c], Tuned: counts[c], Forced: forced})
	}
	servir := effective(&jev.Model{Labels: m.Labels, Thresholds: nuevo}, overrides)
	rep.Before = evalCascade(ep, eg, epr, ew, old)
	rep.After = evalCascade(ep, eg, epr, ew, servir)

	switch {
	case len(ep) < minSupport:
		rep.Reason = fmt.Sprintf("not enough samples: %d in the evaluation half, need at least %d", len(ep), minSupport)
	case rep.Before.Agreement < target && rep.After.Agreement > rep.Before.Agreement:
		rep.Improved = true
		rep.Reason = fmt.Sprintf("agreement on held-out samples %.3f -> %.3f (target %.2f)", rep.Before.Agreement, rep.After.Agreement, target)
	case rep.Before.Agreement >= target && rep.After.Agreement >= target && rep.After.Coverage > rep.Before.Coverage+0.005:
		rep.Improved = true
		rep.Reason = fmt.Sprintf("keeps agreement >= %.2f (%.3f) and raises coverage %.3f -> %.3f", target, rep.After.Agreement, rep.Before.Coverage, rep.After.Coverage)
	default:
		rep.Reason = fmt.Sprintf("no improvement on held-out samples: agreement %.3f -> %.3f, coverage %.3f -> %.3f (target %.2f)",
			rep.Before.Agreement, rep.After.Agreement, rep.Before.Coverage, rep.After.Coverage, target)
	}
	return rep, nuevo
}

// effective son los umbrales con los que se sirve: los del modelo, salvo los
// que la tarea fuerza.
func effective(m *jev.Model, overrides map[string]float64) []float64 {
	out := append([]float64(nil), m.Thresholds...)
	for c, l := range m.Labels {
		if v, ok := overrides[l]; ok && !math.IsNaN(v) {
			out[c] = v
		}
	}
	return out
}
