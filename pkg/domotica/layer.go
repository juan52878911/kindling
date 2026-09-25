package domotica

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// CAPAS ENCHUFABLES.
//
// Las capas 1 y 2 viven en el proceso (Decider) y cuestan microsegundos. Las
// 3 (codificador de frases) y 4 (LLM pequeño, VON) viven en microVMs, cuestan
// milisegundos o segundos y pueden no estar: sin daemon, apagadas o sin una
// evaluación que las respalde. Layer es lo mínimo que tiene que cumplir una de
// ellas para entrar en la Cascade; así el codificador real (otra rama) se
// enchufa sin tocar la cascada ni la demo.

// LayerVON es la capa 4 (Decision.Layer): un LLM pequeño con salida JSON
// restringida. La 3 es LayerEncoder (decide.go).
const LayerVON = "von"

// ReasonInvalidOutput: la respuesta del LLM no pasó la validación estricta
// contra la taxonomía. Se trata como «no hago nada» y se pide aclaración.
const ReasonInvalidOutput = "invalid_output"

// Layer es una capa de la cascada. Decide devuelve una decisión confiada (la
// cascada para ahí) o no confiada (pasa a la siguiente). ErrUnavailable dice
// que la capa no está (sin daemon, sin modelo); otro error, que falló esta
// vez. En los dos casos la cascada sigue con la siguiente capa.
type Layer interface {
	Decide(ctx context.Context, text, lang string) (Decision, error)
}

// ErrUnavailable: la capa no está disponible en este montaje.
var ErrUnavailable = errors.New("layer unavailable")

// ErrDisabled: la capa está, pero su evaluación no la respalda (no gana a lo
// que habría sin ella), así que no decide.
var ErrDisabled = errors.New("layer disabled: its eval does not back it")

// Disabled es una capa presente pero apagada por su evaluación.
var Disabled Layer = LayerFunc(func(context.Context, string, string) (Decision, error) {
	return Decision{}, ErrDisabled
})

type prevKey struct{}

// WithPrev lleva a una capa la decisión de la anterior: la capa 4 la usa para
// no contradecir a Chispa donde Chispa es fuerte (ver VON.Decide).
func WithPrev(ctx context.Context, prev Decision) context.Context {
	return context.WithValue(ctx, prevKey{}, prev)
}

// PrevFrom devuelve la decisión de la capa anterior, si la hay.
func PrevFrom(ctx context.Context) (Decision, bool) {
	d, ok := ctx.Value(prevKey{}).(Decision)
	return d, ok
}

// LayerFunc adapta una función a Layer (mocks, adaptadores).
type LayerFunc func(ctx context.Context, text, lang string) (Decision, error)

// Decide implementa Layer.
func (f LayerFunc) Decide(ctx context.Context, text, lang string) (Decision, error) {
	return f(ctx, text, lang)
}

// Unavailable es la capa que no está: la demo sin daemon la enseña así, y es
// el hueco del codificador hasta que llegue el de verdad.
var Unavailable Layer = LayerFunc(func(context.Context, string, string) (Decision, error) {
	return Decision{}, ErrUnavailable
})

// Action es una acción sobre la habitación: una intención con sus huecos. Una
// orden puede traer varias («apaga todo y cierra la puerta»).
type Action struct {
	Intent string
	Slots  Slots
}

type actionJSON struct {
	Intent string   `json:"intent"`
	Device string   `json:"device,omitempty"`
	Area   string   `json:"area,omitempty"`
	Value  *float64 `json:"value,omitempty"`
	Unit   string   `json:"unit,omitempty"`
	Color  string   `json:"color,omitempty"`
}

// MarshalJSON: {"intent","device","area","value","unit","color"}, plano: es
// la forma que se enseña en la traza de la demo.
func (a Action) MarshalJSON() ([]byte, error) {
	j := actionJSON{Intent: a.Intent, Device: a.Slots.Device, Area: a.Slots.Area, Unit: a.Slots.Unit, Color: a.Slots.Color}
	if a.Slots.HasValue {
		v := a.Slots.Value
		j.Value = &v
	}
	return json.Marshal(j)
}

// UnmarshalJSON es la inversa de MarshalJSON.
func (a *Action) UnmarshalJSON(b []byte) error {
	var j actionJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*a = Action{Intent: j.Intent, Slots: Slots{Device: j.Device, Area: j.Area, Unit: j.Unit, Color: j.Color}}
	if j.Value != nil {
		a.Slots.Value, a.Slots.HasValue = *j.Value, true
	}
	return nil
}

// ActionList es lo que hay que ejecutar: las acciones de la capa 4 si las
// trae, o la intención única de las demás. Nada si la decisión no es confiada
// o es «fuera de ámbito».
func (d Decision) ActionList() []Action {
	if !d.Confident {
		return nil
	}
	if d.Actions != nil {
		return d.Actions
	}
	if d.Intent == "" || d.Intent == OutOfScope {
		return nil
	}
	return []Action{{Intent: d.Intent, Slots: d.Slots}}
}

// Step es el paso de una capa por una orden: lo que la demo enseña en la traza.
type Step struct {
	Layer     string  `json:"layer"`
	Status    string  `json:"status"` // answered | escalated | unavailable | disabled | error | skipped
	Intent    string  `json:"intent,omitempty"`
	Prob      float64 `json:"prob,omitempty"`
	Reason    string  `json:"reason,omitempty"`
	Error     string  `json:"error,omitempty"`
	LatencyUS float64 `json:"latency_us"`
}

// Estados de un paso.
const (
	StepAnswered    = "answered"
	StepEscalated   = "escalated"
	StepUnavailable = "unavailable"
	StepError       = "error"
	StepSkipped     = "skipped"
	StepDisabled    = "disabled"
	StepNoMatch     = "nomatch"    // la plantilla no encajó
	StepNotReached  = "notreached" // una capa anterior ya decidió
)

// Trace es el recorrido de una orden por la cascada.
type Trace struct {
	Text    string   `json:"text"`
	Lang    string   `json:"lang"`
	Steps   []Step   `json:"steps"`
	Final   Decision `json:"final"`
	Actions []Action `json:"actions"`
	Decided string   `json:"decided_by"` // capa que contestó con confianza, o "none"
	TotalUS float64  `json:"total_us"`
}

// NamedLayer es una capa lenta con su nombre, para la traza.
type NamedLayer struct {
	Name  string
	Layer Layer
	// Skip, si no es nil, salta la capa a la vista de la decisión anterior:
	// el codificador devuelve una intención, así que una orden múltiple va
	// directa a VON.
	Skip func(prev Decision) bool
}

// FastFunc son las capas 1 a 3 (plantillas, modelo rápido y codificador): en
// el proceso (InProcess) o por el gateway de IA (/v1/decide).
type FastFunc func(ctx context.Context, text, lang string) (Decision, error)

// InProcess usa un Decider del proceso (con su codificador, si lo tiene).
func InProcess(d *Decider) FastFunc {
	return func(ctx context.Context, text, lang string) (Decision, error) {
		return d.DecideContext(ctx, text, lang), nil
	}
}

// Cascade encadena las capas 1–3 (Fast) con las lentas (la 4). Para en la
// primera respuesta confiada. Si ninguna lo es, la decisión final es la última
// no confiada y la habitación no hace nada: lo mismo que «escalar y ya».
type Cascade struct {
	Fast FastFunc
	// HasEncoder dice si Fast lleva la capa 3 (para la traza: «no disponible»
	// frente a «no hizo falta»).
	HasEncoder bool
	Slow       []NamedLayer
}

// Decide pasa text por la cascada.
func (c *Cascade) Decide(ctx context.Context, text, lang string) Trace {
	t0 := time.Now()
	if lang == "" || lang == "auto" {
		lang = DetectLang(text)
	}
	tr := Trace{Text: text, Lang: lang}
	fast, err := c.Fast(ctx, text, lang)
	if err != nil {
		// Sin las capas rápidas (el gateway no contesta) no se hace nada.
		tr.Steps = []Step{{Layer: LayerTemplate, Status: StepError, Error: err.Error()}}
		tr.Final = Decision{Lang: lang, Layer: LayerNone, Reason: "fast_layers_error"}
		tr.Actions, tr.Decided = []Action{}, "none"
		tr.TotalUS = float64(time.Since(t0).Nanoseconds()) / 1e3
		return tr
	}
	if fast.Lang != "" {
		lang = fast.Lang
		tr.Lang = lang
	}
	tr.Steps = FastSteps(fast, c.HasEncoder)
	final := fast
	if !fast.Confident {
		prev := fast
		for _, l := range c.Slow {
			if l.Skip != nil && l.Skip(prev) {
				tr.Steps = append(tr.Steps, Step{Layer: l.Name, Status: StepSkipped})
				continue
			}
			t1 := time.Now()
			d, err := l.Layer.Decide(WithPrev(ctx, prev), text, lang)
			us := float64(time.Since(t1).Nanoseconds()) / 1e3
			if errors.Is(err, ErrUnavailable) {
				tr.Steps = append(tr.Steps, Step{Layer: l.Name, Status: StepUnavailable})
				continue
			}
			if errors.Is(err, ErrDisabled) {
				tr.Steps = append(tr.Steps, Step{Layer: l.Name, Status: StepDisabled})
				continue
			}
			if err != nil {
				tr.Steps = append(tr.Steps, Step{Layer: l.Name, Status: StepError, Error: err.Error(), LatencyUS: us})
				continue
			}
			if d.Layer == "" {
				d.Layer = l.Name
			}
			d.Lang = lang
			if d.LatencyUS == 0 {
				d.LatencyUS = us
			}
			tr.Steps = append(tr.Steps, stepOf(d))
			prev, final = d, d
			if d.Confident {
				break
			}
		}
	}
	tr.Final = final
	tr.Actions = final.ActionList()
	if tr.Actions == nil {
		tr.Actions = []Action{}
	}
	tr.Decided = "none"
	if final.Confident {
		tr.Decided = final.Layer
	}
	tr.TotalUS = float64(time.Since(t0).Nanoseconds()) / 1e3
	return tr
}

// FastSteps reparte la decisión de las capas 1–3 en un paso por capa. La
// decisión trae lo que hace falta: la capa que contestó, lo que dijo el modelo
// rápido cuando preguntó al codificador (FastIntent, FastProb) y lo que tardó
// este (EncoderUS).
func FastSteps(d Decision, hasEncoder bool) []Step {
	if d.Layer == LayerTemplate {
		return []Step{stepOf(d)}
	}
	steps := []Step{{Layer: LayerTemplate, Status: StepNoMatch}}
	asked := d.EncoderUS > 0 || d.EncoderError != ""
	fastUS := d.LatencyUS - d.EncoderUS
	switch {
	case d.Layer == LayerNone && d.Reason == ReasonNoModel:
		steps = append(steps, Step{Layer: LayerChispa, Status: StepUnavailable})
	case d.Layer == LayerEncoder:
		// Decidió (o dudó) el codificador: el modelo rápido había escalado.
		steps = append(steps, Step{Layer: LayerChispa, Status: StepEscalated, Intent: d.FastIntent, Prob: d.FastProb, LatencyUS: fastUS})
	default:
		s := stepOf(d)
		s.Layer = LayerChispa
		s.LatencyUS = fastUS
		if asked {
			s.Status = StepEscalated
		}
		steps = append(steps, s)
	}
	switch {
	case d.EncoderError != "":
		steps = append(steps, Step{Layer: LayerEncoder, Status: StepError, Error: d.EncoderError, LatencyUS: d.EncoderUS})
	case d.Layer == LayerEncoder:
		s := stepOf(d)
		s.LatencyUS = d.EncoderUS
		steps = append(steps, s)
	case asked:
		// Contestó «fuera de ámbito» o dudó y la conjetura que sigue es la del
		// modelo rápido.
		steps = append(steps, Step{Layer: LayerEncoder, Status: StepEscalated, Reason: d.Reason, LatencyUS: d.EncoderUS})
	case !hasEncoder:
		steps = append(steps, Step{Layer: LayerEncoder, Status: StepUnavailable})
	case d.Confident:
		steps = append(steps, Step{Layer: LayerEncoder, Status: StepNotReached})
	default:
		// Las órdenes múltiples no pasan por el codificador.
		steps = append(steps, Step{Layer: LayerEncoder, Status: StepSkipped})
	}
	return steps
}

func stepOf(d Decision) Step {
	s := Step{Layer: d.Layer, Intent: d.Intent, Prob: d.Prob, Reason: d.Reason, LatencyUS: d.LatencyUS, Status: StepAnswered}
	if !d.Confident {
		s.Status = StepEscalated
	}
	return s
}
