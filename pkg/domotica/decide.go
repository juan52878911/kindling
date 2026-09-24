package domotica

import (
	"time"

	"github.com/juan52878911/kindling/pkg/jev"
	"github.com/juan52878911/kindling/pkg/jev/slots"
)

// Capas de la decisión.
const (
	LayerTemplate = "template" // emparejador de órdenes de la demo
	LayerJEV      = "jev"      // intención JEV + huecos JEV-slots
	LayerNone     = "none"     // ninguna capa rápida contesta
	// EscalateTo es la capa siguiente, que aún no existe aquí: un codificador
	// de frases (MiniLM/e5-small) y detrás un LLM pequeño (VON) con salida
	// JSON. Decision.Escalate la nombra para que el gateway sepa a dónde ir.
	EscalateTo = "encoder"
)

// Motivos por los que una decisión no es confiada.
const (
	ReasonLowProb      = "low_probability" // JEV por debajo del umbral de su clase
	ReasonMissingSlot  = "missing_slot"    // intención clara pero falta el valor o el color
	ReasonMultiCommand = "multi_command"   // «enciende la luz y baja la persiana»
	ReasonNoModel      = "no_model"        // no hay modelo JEV cargado
	ReasonOutOfScope   = "out_of_scope"    // JEV no ve una orden directa; que lo mire el LLM
)

// Decision es lo que devuelve Decide: qué hacer, con qué, qué capa lo decidió
// y si hay que escalar.
type Decision struct {
	Lang      string       `json:"lang"`
	Intent    string       `json:"intent"`
	Slots     Slots        `json:"slots"`
	Layer     string       `json:"layer"`
	Confident bool         `json:"confident"`
	Prob      float64      `json:"prob,omitempty"`
	Reason    string       `json:"reason,omitempty"`
	Escalate  string       `json:"escalate,omitempty"`
	LatencyUS float64      `json:"latency_us"`
	Spans     []slots.Span `json:"spans,omitempty"`
}

// Decider encadena las capas rápidas. Matcher es obligatorio; Intent y Slots
// pueden ser nil (entonces lo que no sea de la demo escala).
type Decider struct {
	Matcher *Matcher
	Intent  *jev.Model
	Slots   *slots.Model
	// NoTemplate salta la capa 1 (para evaluar JEV sola).
	NoTemplate bool
	// FinalOOS da por buena una predicción «fuera de ámbito» confiada. Por
	// defecto no: escala, porque las órdenes indirectas caen ahí (ver
	// docs/DOMOTICA-EVAL.md, frases de reto).
	FinalOOS bool
}

// Campos de JEV por idioma, compartidos y de solo lectura: construir el mapa
// en cada llamada reservaría memoria.
var langFields = map[string]map[string]any{
	"es": {"lang": "es"},
	"en": {"lang": "en"},
}

// Decide decide qué hacer con text. lang "" o "auto" lo detecta.
func (d *Decider) Decide(text, lang string) Decision {
	t0 := time.Now()
	if lang == "" || lang == "auto" {
		lang = DetectLang(text)
	}
	out := d.decide(text, lang)
	out.Lang = lang
	out.LatencyUS = float64(time.Since(t0).Nanoseconds()) / 1e3
	return out
}

func (d *Decider) decide(text, lang string) Decision {
	if !d.NoTemplate {
		if m := d.Matcher.Match(text); m.OK {
			return Decision{Intent: m.Intent, Slots: m.Slots, Layer: LayerTemplate, Confident: true, Prob: 1}
		}
	}
	if d.Intent == nil {
		return Decision{Layer: LayerNone, Reason: ReasonNoModel, Escalate: EscalateTo}
	}
	p := d.Intent.Predict(jev.Input{Text: text, Fields: langFields[lang]})
	out := Decision{Intent: p.Label, Layer: LayerJEV, Prob: p.Prob, Confident: p.Confident}
	if !p.Confident {
		out.Reason = ReasonLowProb
	}
	if p.Label != OutOfScope && d.Slots != nil {
		out.Spans = d.Slots.Tag(text, nil)
		out.Slots = SlotsFromSpans(text, out.Spans)
	}
	out.Slots = Resolve(out.Intent, out.Slots)
	if out.Confident && p.Label == OutOfScope && !d.FinalOOS {
		// «Fuera de ámbito» de JEV significa «no es una orden directa que
		// conozca», y ahí caen también las indirectas («aquí hace frío»), que
		// son justo el trabajo del LLM. Solo un modelo mayor puede decidir que
		// de verdad no hay nada que hacer.
		out.Confident, out.Reason = false, ReasonOutOfScope
	}
	if out.Confident && p.Label != OutOfScope {
		switch {
		case MultiCommand(text):
			out.Confident, out.Reason = false, ReasonMultiCommand
		case !Complete(out.Intent, out.Slots):
			out.Confident, out.Reason = false, ReasonMissingSlot
		}
	}
	if !out.Confident {
		out.Escalate = EscalateTo
	}
	return out
}

// Verbos de orden: dos órdenes unidas por «y»/«and»/«luego» son dos acciones
// y la capa rápida solo sabe devolver una.
var commandVerbs = setOf(
	"enciende", "prende", "apaga", "sube", "baja", "pon", "abre", "cierra", "activa", "desactiva", "ajusta", "cambia",
	"silencia", "pausa", "bloquea", "desbloquea", "arma", "desarma", "reanuda", "aumenta", "reduce",
	"turn", "switch", "set", "open", "close", "lock", "unlock", "dim", "raise", "lower", "pause", "resume", "mute",
	"unmute", "arm", "disarm", "increase", "decrease", "change", "make", "brighten", "play", "stop", "start")

var conjunctions = setOf("y", "e", "luego", "despues", "and", "then", "also", "tambien")

// MultiCommand detecta «verbo … y verbo …». Heurística: sin ella la capa
// rápida ejecutaría solo una de las dos órdenes, con toda confianza.
func MultiCommand(text string) bool {
	toks := slots.Tokenize(text)
	verbBefore := false
	for i, t := range toks {
		if commandVerbs[t.Norm] {
			verbBefore = true
			continue
		}
		if verbBefore && conjunctions[t.Norm] {
			for _, u := range toks[i+1:] {
				if commandVerbs[u.Norm] {
					return true
				}
			}
		}
	}
	return false
}
