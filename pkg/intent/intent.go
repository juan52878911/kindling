// Package intent decide qué orden hay en un texto corto —una intención y sus
// huecos— con capas de coste creciente, y dice a dónde escalar lo que ninguna
// contesta con confianza:
//
//  1. plantillas: órdenes conocidas por coincidencia exacta (microsegundos);
//  2. Chispa: la intención con pkg/chispa y los huecos con pkg/chispa/slots
//     (microsegundos; en el proceso o en una microVM, ver Remote);
//  3. un codificador de frases con una cabeza entrenada (pkg/codificador,
//     milisegundos), si lo hay;
//  4. lo que tampoco resuelve sale con Escalate: "von" (un LLM, fuera de
//     este paquete).
//
// El paquete no sabe nada del dominio: qué plantillas hay, cómo se convierte
// un hueco marcado en un valor, qué necesita una intención para poder
// ejecutarse o cuál es la etiqueta de «fuera de ámbito» los pone un Domain.
// El gateway de IA (pkg/aigw) lo sirve como tarea "intent" en /v1/decide, con
// un Domain de datos (Schema, un fichero JSON) o uno en Go que le pasa el
// programa que lo embebe (aigw.Options.Domains). Ver docs/intent.md.
package intent

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/chispa"
	"github.com/juan52878911/kindling/pkg/chispa/slots"
)

// Capas de la decisión.
const (
	LayerTemplate = "template" // capa 1: plantillas del dominio
	LayerChispa   = "chispa"   // capa 2: intención Chispa + huecos Chispa-slots
	LayerNone     = "none"     // ninguna capa rápida contesta
	LayerEncoder  = "encoder"  // capa 3: codificador de frases + cabeza (pkg/codificador)
	// EscalateTo es la capa siguiente a las rápidas: el codificador de frases
	// (capa 3). Decision.Escalate la nombra para que el gateway sepa a dónde ir.
	EscalateTo = "encoder"
	// EscalateVON es la capa 4, un LLM: a donde va lo que tampoco resuelve el
	// codificador (o lo que no es para él, como dos órdenes en una frase).
	EscalateVON = "von"
)

// Motivos por los que una decisión no es confiada.
const (
	ReasonLowProb      = "low_probability" // Chispa por debajo del umbral de su clase
	ReasonMissingSlot  = "missing_slot"    // intención clara pero le falta un hueco que necesita
	ReasonMultiCommand = "multi_command"   // dos órdenes en una frase: una capa de una etiqueta no basta
	ReasonNoModel      = "no_model"        // no hay modelo Chispa cargado
	ReasonOutOfScope   = "out_of_scope"    // Chispa no ve una orden que conozca; que lo mire la capa siguiente
	ReasonEncoderError = "encoder_error"   // el codificador no contestó (se escala igual)
	ReasonChispaError  = "chispa_error"    // la microVM de Chispa no contestó o mintió (se escala igual)
)

// Domain es lo que el dominio sabe y la cascada no. Tiene que ser seguro para
// uso concurrente.
type Domain interface {
	// OutOfScope es la etiqueta de todo lo que el dominio no sabe hacer.
	OutOfScope() string
	// DetectLang adivina el idioma de text cuando quien llama no lo dice
	// ("" o "auto"). "" = sin idioma (Chispa no recibe el campo lang).
	DetectLang(text string) string
	// Match es la capa 1: una plantilla que encaja exacta, con sus huecos
	// ya completos. ok=false deja decidir a la capa siguiente.
	Match(text string) (intent string, slots any, ok bool)
	// Slots convierte los huecos marcados en text (nil: ninguno) en los
	// valores de la intención, con los implícitos completados. Con una
	// intención desconocida o fuera de ámbito, los huecos vacíos (nunca nil).
	Slots(intent, text string, spans []slots.Span) any
	// Check dice por qué una intención confiada no se puede ejecutar tal
	// cual: "" (se puede), ReasonMultiCommand o ReasonMissingSlot.
	Check(text, intent string, slots any) string
	// ParseSlots lee los huecos de una fila etiquetada (Row.Slots; vacío o
	// null = ninguno).
	ParseSlots(raw json.RawMessage) (any, error)
	// Exact dice si una decisión (intent, got) hace exactamente lo que pide
	// la fila etiquetada (goldIntent, gold de ParseSlots).
	Exact(goldIntent string, gold any, intent string, got any) bool
}

// Decision es lo que devuelve Decide: qué hacer, con qué, qué capa lo decidió
// y si hay que escalar.
type Decision struct {
	Lang      string       `json:"lang"`
	Intent    string       `json:"intent"`
	Slots     any          `json:"slots"`
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
	// sirve serverless (Remote): congelada, pausada o despierta, y lo que
	// costó despertarla. Nil con Chispa en el proceso.
	ChispaReplica *ReplicaInfo `json:"chispa_replica,omitempty"`
	// ChispaError: la microVM de Chispa no contestó (o contestó algo que no
	// pasa la validación del gateway); la orden escala como si dudara.
	ChispaError string `json:"chispa_error,omitempty"`

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
// tenerla lista.
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

// Remote es la capa 2 servida serverless: una réplica de kling-chispa en una
// microVM que el planificador despierta con la orden (backend "microvm" en el
// registro del gateway, docs/chispa-serverless.md).
type Remote interface {
	ClassifyIntent(ctx context.Context, text, lang string) (RemoteAnswer, error)
}

// Encoder es la capa 3: la intención de una frase según un codificador de
// frases y su cabeza (pkg/codificador.Layer). confident es su umbral por
// clase, elegido en validación como el de Chispa.
type Encoder interface {
	ClassifyIntent(ctx context.Context, text string) (intent string, prob float64, confident bool, err error)
}

// Decider encadena las capas. Domain es obligatorio; Intent y Slots pueden ser
// nil (entonces lo que no sea una plantilla escala).
type Decider struct {
	Domain Domain
	Intent *chispa.Model
	// Remote es la capa 2 en una microVM, en vez de Intent en el proceso
	// (Intent gana si están los dos). Sus huecos se usan si Slots es nil y la
	// réplica los trae.
	Remote Remote
	Slots  *slots.Model
	// NoTemplate salta la capa 1 (para evaluar Chispa sola).
	NoTemplate bool
	// FinalOOS da por buena una predicción «fuera de ámbito» confiada. Por
	// defecto no: escala, porque las órdenes indirectas («aquí hace frío»)
	// caen ahí.
	FinalOOS bool
	// Encoder es la capa 3 (nil = no hay: lo no confiado escala a
	// "encoder"). Con ella, lo que tampoco resuelve escala a "von".
	Encoder Encoder
	// OnlyEncoder salta las capas 1 y 2 y pregunta siempre al codificador
	// (para evaluarlo solo).
	OnlyEncoder bool
}

// Campos de Chispa por idioma, compartidos y de solo lectura: construir el
// mapa en cada llamada reservaría memoria.
var langFields sync.Map // string -> map[string]any

// LangFields son los campos de Chispa de un idioma (nil sin idioma). El mapa
// es compartido: de solo lectura.
func LangFields(lang string) map[string]any {
	if lang == "" {
		return nil
	}
	if f, ok := langFields.Load(lang); ok {
		return f.(map[string]any)
	}
	f, _ := langFields.LoadOrStore(lang, map[string]any{"lang": lang})
	return f.(map[string]any)
}

// Decide decide qué hacer con text. lang "" o "auto" lo detecta.
func (d *Decider) Decide(text, lang string) Decision {
	return d.DecideContext(context.Background(), text, lang)
}

// DecideContext es Decide con un contexto para las capas que son una
// petición a una réplica (la 2 serverless y la 3).
func (d *Decider) DecideContext(ctx context.Context, text, lang string) Decision {
	t0 := time.Now()
	if lang == "" || lang == "auto" {
		lang = d.Domain.DetectLang(text)
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
	if out.Slots == nil {
		// Sin intención (o fuera de ámbito): los huecos vacíos del dominio,
		// no null.
		out.Slots = d.Domain.Slots(out.Intent, text, nil)
	}
	out.Lang = lang
	out.LatencyUS = float64(time.Since(t0).Nanoseconds()) / 1e3
	return out
}

func (d *Decider) decide(ctx context.Context, text, lang string) Decision {
	if !d.NoTemplate {
		if it, s, ok := d.Domain.Match(text); ok {
			return Decision{Intent: it, Slots: s, Layer: LayerTemplate, Confident: true, Prob: 1}
		}
	}
	oos := d.Domain.OutOfScope()
	var (
		p      chispa.Prediction
		out    Decision
		remote *RemoteAnswer
	)
	switch {
	case d.Intent != nil:
		p = d.Intent.Predict(chispa.Input{Text: text, Fields: LangFields(lang)})
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
		if p.Label != oos {
			out.Spans = d.Slots.Tag(text, nil)
		}
	case remote != nil && remote.HasSlots:
		if p.Label != oos {
			out.Spans = remote.Spans
		} else {
			out.remoteSpans = remote.Spans
		}
	}
	out.Slots = d.Domain.Slots(out.Intent, text, out.Spans)
	if out.Confident && p.Label == oos && !d.FinalOOS {
		// «Fuera de ámbito» de Chispa significa «no es una orden directa que
		// conozca», y ahí caen también las indirectas, que son justo el
		// trabajo de un modelo mayor. Solo él puede decidir que de verdad no
		// hay nada que hacer.
		out.Confident, out.Reason = false, ReasonOutOfScope
	}
	if out.Confident && p.Label != oos {
		if r := d.Domain.Check(text, out.Intent, out.Slots); r != "" {
			out.Confident, out.Reason = false, r
		}
	}
	if !out.Confident {
		out.Escalate = EscalateTo
	}
	return out
}

// encode es la capa 3 sobre una decisión no confiada de las rápidas.
//
// Qué se le pregunta y qué no: dos órdenes en una frase no las arregla un
// clasificador de una etiqueta, así que van directas a VON sin gastar los
// milisegundos del codificador. Todo lo demás (Chispa por debajo de su umbral,
// «fuera de ámbito», un hueco que falta porque quizá la intención era otra)
// pasa por él. Si contesta confiado una orden del dominio, con sus huecos
// completos, esa es la decisión; si no, escala a VON con la mejor conjetura
// de las dos capas.
func (d *Decider) encode(ctx context.Context, text string, fast Decision) Decision {
	fast.Escalate = EscalateVON
	if fast.Reason == ReasonMultiCommand {
		return fast
	}
	oos := d.Domain.OutOfScope()
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
	if intent == oos {
		if confident && d.FinalOOS {
			return out
		}
		// Igual que en Chispa: «fuera de ámbito» es «no es una orden que
		// conozca», y ahí están las indirectas. Lo decide VON.
		out.Confident, out.Reason, out.Escalate = false, ReasonOutOfScope, EscalateVON
		// La conjetura que queda es la de las rápidas si veían una orden.
		if fast.Intent != "" && fast.Intent != oos {
			out.Intent, out.Slots, out.Spans, out.Prob = fast.Intent, fast.Slots, fast.Spans, fast.Prob
		}
		return out
	}
	// Los huecos los sigue marcando Chispa-slots; si Chispa dijo «fuera de
	// ámbito» no los había buscado.
	out.Spans = fast.Spans
	switch {
	case out.Spans != nil:
	case d.Slots != nil:
		out.Spans = d.Slots.Tag(text, nil)
	case fast.remoteSpans != nil:
		out.Spans = fast.remoteSpans
	}
	out.Slots = d.Domain.Slots(intent, text, out.Spans)
	if !confident {
		out.Reason = ReasonLowProb
	} else if r := d.Domain.Check(text, out.Intent, out.Slots); r != "" {
		out.Confident, out.Reason = false, r
	}
	if !out.Confident {
		out.Escalate = EscalateVON
		// La conjetura que se escala es la de la capa más segura de las dos:
		// con la del codificador siempre, lo fuera de ámbito que Chispa
		// acierta se convertía en órdenes (docs/codificador.md).
		if fast.Intent != "" && fast.Prob > out.Prob {
			out.Intent, out.Slots, out.Spans, out.Prob = fast.Intent, fast.Slots, fast.Spans, fast.Prob
		}
	}
	return out
}
