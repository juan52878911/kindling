package domotica

import (
	"context"
	"encoding/json"

	"github.com/juan52878911/kindling/pkg/chispa"
	"github.com/juan52878911/kindling/pkg/chispa/slots"
	"github.com/juan52878911/kindling/pkg/intent"
)

// La cascada de capas es la genérica de kindling (pkg/intent), la misma que
// sirve el gateway de IA en /v1/decide: aquí solo está lo que sabe de la
// habitación (Domain: plantillas, léxico, taxonomía) y la Decision de la
// demo, que añade lo de la capa 4.

// Capas de la decisión (pkg/intent).
const (
	LayerTemplate = intent.LayerTemplate // emparejador de órdenes de la demo
	LayerChispa   = intent.LayerChispa   // intención Chispa + huecos Chispa-slots
	LayerNone     = intent.LayerNone     // ninguna capa rápida contesta
	LayerEncoder  = intent.LayerEncoder  // capa 3: codificador de frases + cabeza (pkg/codificador)
	EscalateTo    = intent.EscalateTo    // la capa 3
	EscalateVON   = intent.EscalateVON   // la capa 4: un LLM pequeño con salida JSON
)

// Motivos por los que una decisión no es confiada (o, en la capa 4, por los
// que no hace nada).
const (
	// ReasonVetoedByChispa: Chispa dijo con confianza «no es una orden directa
	// que conozca» y el LLM propone una orden directa: gana Chispa (ver VON.Decide).
	ReasonVetoedByChispa = "chispa_veto"
	ReasonLowProb        = intent.ReasonLowProb      // Chispa por debajo del umbral de su clase
	ReasonMissingSlot    = intent.ReasonMissingSlot  // intención clara pero falta el valor o el color
	ReasonMultiCommand   = intent.ReasonMultiCommand // «enciende la luz y baja la persiana»
	ReasonNoModel        = intent.ReasonNoModel      // no hay modelo Chispa cargado
	ReasonOutOfScope     = intent.ReasonOutOfScope   // Chispa no ve una orden directa; que lo mire el LLM
	ReasonEncoderError   = intent.ReasonEncoderError // el codificador no contestó (se escala igual)
	ReasonChispaError    = intent.ReasonChispaError  // la microVM de Chispa no contestó o mintió (se escala igual)
)

// Decision es lo que devuelve Decide: qué hacer, con qué, qué capa lo decidió
// y si hay que escalar. Es la de pkg/intent (el mismo JSON que devuelve el
// gateway en /v1/decide) con los huecos de la habitación y lo de la capa 4.
type Decision struct {
	Lang          string       `json:"lang"`
	Intent        string       `json:"intent"`
	Slots         Slots        `json:"slots"`
	Layer         string       `json:"layer"`
	Confident     bool         `json:"confident"`
	Prob          float64      `json:"prob,omitempty"`
	Reason        string       `json:"reason,omitempty"`
	Escalate      string       `json:"escalate,omitempty"`
	LatencyUS     float64      `json:"latency_us"`
	Spans         []slots.Span `json:"spans,omitempty"`
	FastIntent    string       `json:"fast_intent,omitempty"`
	FastProb      float64      `json:"fast_prob,omitempty"`
	EncoderUS     float64      `json:"encoder_us,omitempty"`
	EncoderError  string       `json:"encoder_error,omitempty"`
	ChispaReplica *ReplicaInfo `json:"chispa_replica,omitempty"`
	ChispaError   string       `json:"chispa_error,omitempty"`
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
}

// FromIntent convierte una decisión de pkg/intent tomada con Domain.
func FromIntent(g intent.Decision) Decision {
	s, _ := g.Slots.(Slots)
	return Decision{Lang: g.Lang, Intent: g.Intent, Slots: s, Layer: g.Layer, Confident: g.Confident,
		Prob: g.Prob, Reason: g.Reason, Escalate: g.Escalate, LatencyUS: g.LatencyUS, Spans: g.Spans,
		FastIntent: g.FastIntent, FastProb: g.FastProb, EncoderUS: g.EncoderUS, EncoderError: g.EncoderError,
		ChispaReplica: g.ChispaReplica, ChispaError: g.ChispaError}
}

// Estados de la microVM de una capa al llegar una orden (ReplicaInfo.State).
const (
	ReplicaFrozen = intent.ReplicaFrozen // congelada en disco: se descongeló (thaw)
	ReplicaPaused = intent.ReplicaPaused // pausada en memoria: se reanudó (resume)
	ReplicaWarm   = intent.ReplicaWarm   // ya estaba despierta
	ReplicaNew    = intent.ReplicaNew    // no había ninguna: se restauró del dorado
)

// Tipos de la cascada genérica.
type (
	// ReplicaInfo es cómo estaba la microVM que sirvió una capa y lo que
	// costó tenerla lista: la traza de la demo lo enseña.
	ReplicaInfo = intent.ReplicaInfo
	// RemoteAnswer es lo que contesta una Chispa servida en una microVM.
	RemoteAnswer = intent.RemoteAnswer
	// RemoteIntent es la capa 2 servida serverless.
	RemoteIntent = intent.Remote
	// IntentEncoder es la capa 3 (pkg/codificador.Layer).
	IntentEncoder = intent.Encoder
)

// Decider encadena las capas rápidas de la habitación. Matcher es
// obligatorio; Intent y Slots pueden ser nil (entonces lo que no sea de la
// demo escala). Los campos son los de intent.Decider.
type Decider struct {
	Matcher    *Matcher
	Intent     *chispa.Model
	Remote     RemoteIntent
	Slots      *slots.Model
	NoTemplate bool
	// FinalOOS da por buena una predicción «fuera de ámbito» confiada. Por
	// defecto no: escala, porque las órdenes indirectas caen ahí (ver
	// docs/DOMOTICA-EVAL.md, frases de reto).
	FinalOOS    bool
	Encoder     IntentEncoder
	OnlyEncoder bool
}

// Generic es la cascada de pkg/intent con el dominio de la habitación.
func (d *Decider) Generic() *intent.Decider {
	return &intent.Decider{Domain: Domain{Matcher: d.Matcher}, Intent: d.Intent, Remote: d.Remote, Slots: d.Slots,
		NoTemplate: d.NoTemplate, FinalOOS: d.FinalOOS, Encoder: d.Encoder, OnlyEncoder: d.OnlyEncoder}
}

// Decide decide qué hacer con text. lang "" o "auto" lo detecta.
func (d *Decider) Decide(text, lang string) Decision {
	return d.DecideContext(context.Background(), text, lang)
}

// DecideContext es Decide con un contexto para la capa 3, que es una
// petición a una réplica (las capas rápidas no lo miran).
func (d *Decider) DecideContext(ctx context.Context, text, lang string) Decision {
	return FromIntent(d.Generic().DecideContext(ctx, text, lang))
}

// Domain es la habitación de demo para pkg/intent: las plantillas de la demo
// (capa 1), el léxico que convierte huecos en valores, la taxonomía (qué
// completa y qué necesita cada intención) y las órdenes múltiples. Es lo que
// el gateway de la demo (kindling-domotica gateway) registra como dominio
// "smart-room".
type Domain struct {
	Matcher *Matcher
}

var _ intent.Domain = Domain{}

// OutOfScope implementa intent.Domain.
func (Domain) OutOfScope() string { return OutOfScope }

// DetectLang implementa intent.Domain.
func (Domain) DetectLang(text string) string { return DetectLang(text) }

// Match implementa intent.Domain: la capa 1.
func (d Domain) Match(text string) (string, any, bool) {
	if d.Matcher == nil {
		return "", nil, false
	}
	m := d.Matcher.Match(text)
	if !m.OK {
		return "", nil, false
	}
	return m.Intent, m.Slots, true
}

// Slots implementa intent.Domain.
func (Domain) Slots(intentName, text string, spans []slots.Span) any {
	return Resolve(intentName, SlotsFromSpans(text, spans))
}

// Check implementa intent.Domain.
func (Domain) Check(text, intentName string, s any) string {
	sl, _ := s.(Slots)
	switch {
	case MultiCommand(text):
		return ReasonMultiCommand
	case !Complete(intentName, sl):
		return ReasonMissingSlot
	}
	return ""
}

// ParseSlots implementa intent.Domain.
func (Domain) ParseSlots(raw json.RawMessage) (any, error) {
	var s Slots
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Exact implementa intent.Domain: la orden completa (intención y huecos, con
// los implícitos completados igual en los dos lados).
func (Domain) Exact(goldIntent string, gold any, intentName string, got any) bool {
	g, _ := gold.(Slots)
	p, _ := got.(Slots)
	return intentName == goldIntent && SlotsEqual(Resolve(goldIntent, g), Resolve(intentName, p))
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
