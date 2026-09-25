package domotica

import (
	"context"
	"time"

	"github.com/juan52878911/kindling/pkg/chispa"
	"github.com/juan52878911/kindling/pkg/chispa/slots"
)

// Capas de la decisión.
const (
	LayerTemplate = "template" // emparejador de órdenes de la demo
	LayerChispa   = "chispa"   // intención Chispa + huecos Chispa-slots
	LayerNone     = "none"     // ninguna capa rápida contesta
	LayerEncoder  = "encoder"  // capa 3: codificador de frases + cabeza (pkg/codificador)
	// EscalateTo es la capa siguiente a las rápidas: el codificador de frases
	// (capa 3). Decision.Escalate la nombra para que el gateway sepa a dónde ir.
	EscalateTo = "encoder"
	// EscalateVON es la capa 4, un LLM pequeño con salida JSON: a donde va lo
	// que tampoco resuelve el codificador (o lo que no es para él, como dos
	// órdenes en una frase).
	EscalateVON = "von"
)

// Motivos por los que una decisión no es confiada (o, en la capa 4, por los
// que no hace nada).
const (
	// ReasonVetoedByChispa: Chispa dijo con confianza «no es una orden directa
	// que conozca» y el LLM propone una orden directa: gana Chispa (ver VON.Decide).
	ReasonVetoedByChispa = "chispa_veto"
	ReasonLowProb        = "low_probability" // Chispa por debajo del umbral de su clase
	ReasonMissingSlot    = "missing_slot"    // intención clara pero falta el valor o el color
	ReasonMultiCommand   = "multi_command"   // «enciende la luz y baja la persiana»
	ReasonNoModel        = "no_model"        // no hay modelo Chispa cargado
	ReasonOutOfScope     = "out_of_scope"    // Chispa no ve una orden directa; que lo mire el LLM
	ReasonEncoderError   = "encoder_error"   // el codificador no contestó (se escala igual)
	ReasonChispaError    = "chispa_error"    // la microVM de Chispa no contestó o mintió (se escala igual)
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
	// Lo que dijeron las capas rápidas cuando decidió (o se consultó) la capa
	// 3, y lo que tardó el codificador: para leer una decisión sin repetirla.
	FastIntent   string  `json:"fast_intent,omitempty"`
	FastProb     float64 `json:"fast_prob,omitempty"`
	EncoderUS    float64 `json:"encoder_us,omitempty"`
	EncoderError string  `json:"encoder_error,omitempty"`
	// ChispaReplica es cómo estaba la microVM de Chispa cuando la capa 2 se
	// sirve serverless (RemoteIntent): congelada, pausada o despierta, y lo
	// que costó despertarla. Nil con Chispa en el proceso.
	ChispaReplica *ReplicaInfo `json:"chispa_replica,omitempty"`
	// ChispaError: la microVM de Chispa no contestó (o contestó algo que no
	// pasa la validación del gateway); la orden escala como si dudara.
	ChispaError string `json:"chispa_error,omitempty"`
	// Actions: la lista de acciones cuando la capa sabe devolver varias (la 4:
	// «apaga la luz y cierra la puerta»). Vacía en las capas de una intención;
	// Intent/Slots son entonces la primera.
	Actions []Action `json:"actions,omitempty"`
	// Reply es la frase corta que la capa 4 propone decir de vuelta (o la
	// pregunta de aclaración cuando no entendió).
	Reply string `json:"reply,omitempty"`
	// Model es el modelo que decidió en las capas lentas.
	Model string    `json:"model,omitempty"`
	Kind  string    `json:"kind,omitempty"` // capa 4: command | situation | other
	LLM   *LLMStats `json:"llm,omitempty"`

	// remoteSpans son los huecos que la réplica de Chispa marcó aunque dijera
	// «fuera de ámbito» (kling-chispa los manda siempre): la capa 3 los usa si
	// cambia la intención, sin volver a preguntar. No salen en el JSON.
	remoteSpans []slots.Span
}

// Estados de la microVM de una capa al llegar una orden (ReplicaInfo.State).
const (
	ReplicaFrozen = "frozen" // congelada en disco: se descongeló (thaw)
	ReplicaPaused = "paused" // pausada en memoria: se reanudó (resume)
	ReplicaWarm   = "warm"   // ya estaba despierta
	ReplicaNew    = "new"    // no había ninguna: se restauró del dorado
)

// ReplicaInfo es cómo estaba la microVM que sirvió una capa y lo que costó
// tenerla lista: la traza de la demo lo enseña junto a la latencia de la capa.
type ReplicaInfo struct {
	// Model es el modelo del registro del gateway.
	Model string `json:"model"`
	// State: frozen | paused | warm | new.
	State string `json:"state"`
	// WakeMS es lo que tardó en estar lista (0 si ya lo estaba).
	WakeMS float64 `json:"wake_ms,omitempty"`
	// RequestMS es la petición a la réplica ya despierta, ida y vuelta.
	RequestMS float64 `json:"request_ms"`
}

// RemoteAnswer es lo que contesta una Chispa servida en una microVM, ya
// validado por quien la llama (pkg/aigw): etiqueta y probabilidad del
// registro de despliegue, confianza calculada del lado del gateway y huecos
// dentro del texto.
type RemoteAnswer struct {
	Label     string
	Prob      float64
	Confident bool
	// HasSlots dice si la réplica lleva modelo de huecos (kling chispa deploy
	// -slots); entonces Spans son sus huecos (quizá ninguno).
	HasSlots bool
	Spans    []slots.Span
	Replica  *ReplicaInfo
}

// RemoteIntent es la capa 2 servida serverless: una réplica de kling-chispa
// en una microVM que el planificador despierta con la orden (backend
// "microvm" en el registro del gateway, docs/chispa-serverless.md).
type RemoteIntent interface {
	ClassifyIntent(ctx context.Context, text, lang string) (RemoteAnswer, error)
}

// IntentEncoder es la capa 3: la intención de una frase según un codificador
// de frases y su cabeza (pkg/codificador.Layer). confident es su umbral por
// clase, elegido en validación como el de Chispa.
type IntentEncoder interface {
	ClassifyIntent(ctx context.Context, text string) (intent string, prob float64, confident bool, err error)
}

// Decider encadena las capas rápidas. Matcher es obligatorio; Intent y Slots
// pueden ser nil (entonces lo que no sea de la demo escala).
type Decider struct {
	Matcher *Matcher
	Intent  *chispa.Model
	// Remote es la capa 2 en una microVM, en vez de Intent en el proceso
	// (Intent gana si están los dos). Sus huecos se usan si Slots es nil y la
	// réplica los trae.
	Remote RemoteIntent
	Slots  *slots.Model
	// NoTemplate salta la capa 1 (para evaluar Chispa sola).
	NoTemplate bool
	// FinalOOS da por buena una predicción «fuera de ámbito» confiada. Por
	// defecto no: escala, porque las órdenes indirectas caen ahí (ver
	// docs/DOMOTICA-EVAL.md, frases de reto).
	FinalOOS bool
	// Encoder es la capa 3 (nil = no hay: lo no confiado escala a
	// "encoder"). Con ella, lo que tampoco resuelve escala a "von".
	Encoder IntentEncoder
	// OnlyEncoder salta las capas 1 y 2 y pregunta siempre al codificador
	// (para evaluarlo solo).
	OnlyEncoder bool
}

// Campos de Chispa por idioma, compartidos y de solo lectura: construir el mapa
// en cada llamada reservaría memoria.
var langFields = map[string]map[string]any{
	"es": {"lang": "es"},
	"en": {"lang": "en"},
}

// Decide decide qué hacer con text. lang "" o "auto" lo detecta.
func (d *Decider) Decide(text, lang string) Decision {
	return d.DecideContext(context.Background(), text, lang)
}

// DecideContext es Decide con un contexto para la capa 3, que es una
// petición a una réplica (las capas rápidas no lo miran).
func (d *Decider) DecideContext(ctx context.Context, text, lang string) Decision {
	t0 := time.Now()
	if lang == "" || lang == "auto" {
		lang = DetectLang(text)
	}
	var out Decision
	if d.OnlyEncoder {
		out = Decision{Layer: LayerNone, Reason: ReasonNoModel, Escalate: EscalateTo}
	} else {
		out = d.decide(ctx, text, lang)
	}
	if !out.Confident && d.Encoder != nil {
		out = d.encode(ctx, text, out)
	}
	out.Lang = lang
	out.LatencyUS = float64(time.Since(t0).Nanoseconds()) / 1e3
	return out
}

func (d *Decider) decide(ctx context.Context, text, lang string) Decision {
	if !d.NoTemplate {
		if m := d.Matcher.Match(text); m.OK {
			return Decision{Intent: m.Intent, Slots: m.Slots, Layer: LayerTemplate, Confident: true, Prob: 1}
		}
	}
	var (
		p      chispa.Prediction
		out    Decision
		remote *RemoteAnswer
	)
	switch {
	case d.Intent != nil:
		p = d.Intent.Predict(chispa.Input{Text: text, Fields: langFields[lang]})
	case d.Remote != nil:
		a, err := d.Remote.ClassifyIntent(ctx, text, lang)
		if err != nil {
			return Decision{Layer: LayerNone, Reason: ReasonChispaError, Escalate: EscalateTo,
				ChispaError: err.Error(), ChispaReplica: a.Replica}
		}
		remote = &a
		p = chispa.Prediction{Label: a.Label, Prob: a.Prob, Confident: a.Confident}
		out.ChispaReplica = a.Replica
	default:
		return Decision{Layer: LayerNone, Reason: ReasonNoModel, Escalate: EscalateTo}
	}
	out.Intent, out.Layer, out.Prob, out.Confident = p.Label, LayerChispa, p.Prob, p.Confident
	if !p.Confident {
		out.Reason = ReasonLowProb
	}
	switch {
	case d.Slots != nil:
		if p.Label != OutOfScope {
			out.Spans = d.Slots.Tag(text, nil)
		}
	case remote != nil && remote.HasSlots:
		if p.Label != OutOfScope {
			out.Spans = remote.Spans
		} else {
			out.remoteSpans = remote.Spans
		}
	}
	if out.Spans != nil {
		out.Slots = SlotsFromSpans(text, out.Spans)
	}
	out.Slots = Resolve(out.Intent, out.Slots)
	if out.Confident && p.Label == OutOfScope && !d.FinalOOS {
		// «Fuera de ámbito» de Chispa significa «no es una orden directa que
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

// encode es la capa 3 sobre una decisión no confiada de las rápidas.
//
// Qué se le pregunta y qué no: dos órdenes en una frase no las arregla un
// clasificador de una etiqueta, así que van directas a VON sin gastar los
// milisegundos del codificador. Todo lo demás (Chispa por debajo de su umbral,
// «fuera de ámbito», un hueco que falta porque quizá la intención era otra)
// pasa por él. Si contesta confiado una orden de la habitación, con sus
// huecos completos, esa es la decisión; si no, escala a VON con la mejor
// conjetura de las dos capas.
func (d *Decider) encode(ctx context.Context, text string, fast Decision) Decision {
	fast.Escalate = EscalateVON
	if fast.Reason == ReasonMultiCommand {
		return fast
	}
	t0 := time.Now()
	intent, prob, confident, err := d.Encoder.ClassifyIntent(ctx, text)
	us := float64(time.Since(t0).Nanoseconds()) / 1e3
	fast.EncoderUS = us
	if err != nil {
		fast.EncoderError = err.Error()
		if fast.Reason == "" || fast.Reason == ReasonNoModel {
			fast.Reason = ReasonEncoderError
		}
		return fast
	}
	out := Decision{Intent: intent, Layer: LayerEncoder, Prob: prob, Confident: confident,
		FastIntent: fast.Intent, FastProb: fast.Prob, EncoderUS: us,
		ChispaReplica: fast.ChispaReplica, ChispaError: fast.ChispaError}
	if intent == OutOfScope {
		if confident && d.FinalOOS {
			return out
		}
		// Igual que en Chispa: «fuera de ámbito» es «no es una orden que
		// conozca», y ahí están las indirectas. Lo decide VON.
		out.Confident, out.Reason, out.Escalate = false, ReasonOutOfScope, EscalateVON
		// La conjetura que queda es la de las rápidas si veían una orden.
		if fast.Intent != "" && fast.Intent != OutOfScope {
			out.Intent, out.Slots, out.Spans, out.Prob = fast.Intent, fast.Slots, fast.Spans, fast.Prob
		}
		return out
	}
	// Los huecos los sigue marcando Chispa-slots (F1 0,99 en lo generado por
	// gramática); si Chispa dijo «fuera de ámbito» no los había buscado.
	out.Spans = fast.Spans
	switch {
	case out.Spans != nil:
	case d.Slots != nil:
		out.Spans = d.Slots.Tag(text, nil)
	case fast.remoteSpans != nil:
		out.Spans = fast.remoteSpans
	}
	out.Slots = Resolve(intent, SlotsFromSpans(text, out.Spans))
	switch {
	case !confident:
		out.Reason = ReasonLowProb
	case MultiCommand(text):
		out.Confident, out.Reason = false, ReasonMultiCommand
	case !Complete(out.Intent, out.Slots):
		out.Confident, out.Reason = false, ReasonMissingSlot
	}
	if !out.Confident {
		out.Escalate = EscalateVON
		// La conjetura que se escala es la de la capa más segura de las dos:
		// elegido en validación frente a «siempre la del codificador» y
		// «siempre la de Chispa» (docs/codificador.md). Con la del codificador
		// siempre, lo fuera de ámbito que Chispa acierta se convertía en órdenes.
		if fast.Intent != "" && fast.Prob > out.Prob {
			out.Intent, out.Slots, out.Spans, out.Prob = fast.Intent, fast.Slots, fast.Spans, fast.Prob
		}
	}
	return out
}
