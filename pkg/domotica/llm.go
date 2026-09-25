package domotica

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/juan52878911/kindling/pkg/chispa/slots"
)

// CAPA 4: UN LLM PEQUEÑO (VON) PARA LO QUE LAS CAPAS RÁPIDAS NO SABEN.
//
// Lo indirecto («aquí hace frío»), varias órdenes en una frase y los valores
// relativos («a la mitad») los escala la cascada rápida. Aquí se le pregunta a
// un LLM instruct de 0,5–1,5B parámetros servido por kindling (la tarea de
// generación del gateway de IA, POST /v1/generate, que despierta la réplica)
// con dos seguros:
//
//  1. Un esquema JSON que llama-server convierte en gramática: la salida es,
//     por construcción, {"actions":[{"intent","device","area","value",
//     "color"}], "reply"} con la intención dentro de la taxonomía.
//  2. Una validación estricta en el host, porque el invitado no es de fiar
//     (el esquema puede no aplicarse, el modelo puede cambiar): intención
//     conocida, dispositivo compatible, valor dentro de rango, huecos
//     obligatorios. Si algo falla, no se ejecuta NADA de la respuesta y se
//     pide aclaración: una acción inventada en una casa es peor que ninguna.
//
// El mensaje de sistema es fijo (la taxonomía, las reglas y los ejemplos) y el
// estado de la habitación va en el mensaje del usuario: llama-server reutiliza
// la caché KV del prefijo común, así que en caliente solo se evalúan los
// tokens del estado y de la orden.

// LLMStats es lo que se guarda de la llamada al LLM para la traza.
type LLMStats struct {
	Raw string `json:"raw,omitempty"` // la respuesta tal cual, recortada
}

// Generator pide una generación al LLM: la tarea de generación del gateway de
// IA (POST /v1/generate), con LLMSystemPrompt como sistema, "{input}" como
// plantilla, LLMSchema como json_schema y temperatura 0. Devuelve el texto y
// el modelo que contestó. ErrInvalidJSON dice que el modelo no devolvió JSON
// (el gateway lo comprueba): cuenta como una respuesta inválida, no como un
// fallo de la capa.
type Generator func(ctx context.Context, input string) (output, model string, err error)

// ErrInvalidJSON: el LLM contestó, pero no con JSON.
var ErrInvalidJSON = errors.New("the LLM did not return valid JSON")

// VON es la capa 4.
type VON struct {
	Generate Generator
	// State describe el estado de la habitación para el prompt; nil = sin
	// estado (lo evaluado: con estado, el modelo inventa zonas).
	State func() string
	// Timeout de una decisión (incluye despertar la réplica). 0 = 60 s.
	Timeout time.Duration
}

// LLMInput es el mensaje del usuario para una orden: lo que va en {input}.
func LLMInput(text, state string) string {
	in := "Request: " + text
	if state != "" {
		in = "Room state: " + state + "\n" + in
	}
	return in
}

// Topes de la respuesta que se valida y de la frase de vuelta.
const (
	maxLLMOutput = 16 << 10
	maxReplyRune = 200
	maxActions   = 4
)

// Decide implementa Layer.
func (v *VON) Decide(ctx context.Context, text, lang string) (Decision, error) {
	if v == nil || v.Generate == nil {
		return Decision{}, ErrUnavailable
	}
	if lang == "" || lang == "auto" {
		lang = DetectLang(text)
	}
	t0 := time.Now()
	to := v.Timeout
	if to <= 0 {
		to = 60 * time.Second
	}
	gctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	state := ""
	if v.State != nil {
		state = v.State()
	}
	out, model, err := v.Generate(gctx, LLMInput(text, state))
	switch {
	case errors.Is(err, ErrInvalidJSON):
		out = ""
	case err != nil:
		return Decision{}, err
	}
	if len(out) > maxLLMOutput {
		out = ""
	}
	d := ParseLLM(out, text, lang)
	d.Layer, d.Lang, d.Model = LayerVON, lang, model
	d.LLM = &LLMStats{Raw: clip(out, 600)}
	if prev, ok := PrevFrom(ctx); ok {
		d = Veto(prev, d, text, lang)
	}
	d.LatencyUS = float64(time.Since(t0).Nanoseconds()) / 1e3
	return d, nil
}

// Veto aplica a la respuesta del LLM lo que ya sabían las capas anteriores.
//
// Chispa reconoce las órdenes directas de la habitación con un 99 % de acierto.
// Si dio «fuera de ámbito» con confianza, aquí solo cabe lo indirecto: una
// frase que describe cómo está algo («aquí hace frío»), sin verbo de orden.
// Una en imperativo («pon una alarma a las siete», «enciende la cafetera») es
// una orden, pero no de esta habitación, y el LLM pequeño tiende a encajarla
// en la intención más parecida. Medido en MASSIVE: sin este veto actúa en un
// 10 % de lo que no es de la habitación.
func Veto(prev, d Decision, text, lang string) Decision {
	if prev.Reason != ReasonOutOfScope || len(d.Actions) == 0 || (d.Kind == KindSituation && !hasCommandVerb(text)) {
		return d
	}
	return Decision{Layer: d.Layer, Lang: lang, Model: d.Model, Confident: true, Intent: OutOfScope, Actions: []Action{},
		Reason: ReasonVetoedByChispa, Kind: d.Kind, Reply: cannot(lang), LLM: d.LLM}
}

// ParseLLM valida la respuesta del LLM contra la taxonomía. Una respuesta
// válida es confiada (con cero acciones si no hay nada que hacer); una
// inválida no ejecuta nada y pide aclaración.
func ParseLLM(content, text, lang string) Decision {
	acts, kind, reply, err := parseActions(content, text)
	if err != nil {
		return Decision{Intent: OutOfScope, Reason: ReasonInvalidOutput, Reply: clarify(lang)}
	}
	d := Decision{Confident: true, Actions: acts, Reply: reply, Kind: kind}
	if len(acts) == 0 {
		d.Intent = OutOfScope
		if d.Reply == "" {
			d.Reply = cannot(lang)
		}
		return d
	}
	d.Intent, d.Slots = acts[0].Intent, acts[0].Slots
	return d
}

func clarify(lang string) string {
	if lang == "en" {
		return "Sorry, I didn't get that. Could you say it another way?"
	}
	return "Perdona, no te he entendido. ¿Puedes decirlo de otra forma?"
}

func cannot(lang string) string {
	if lang == "en" {
		return "I can't do that in this room."
	}
	return "Eso no lo puedo hacer en esta habitación."
}

// Clases de frase que el LLM declara antes de las acciones.
const (
	KindCommand   = "command"   // una o varias órdenes directas
	KindSituation = "situation" // lenguaje indirecto: describe cómo está algo
	KindOther     = "other"     // nada que esta habitación pueda hacer
)

type llmAction struct {
	Intent string   `json:"intent"`
	Device *string  `json:"device"`
	Area   *string  `json:"area"`
	Value  *float64 `json:"value"`
	Color  *string  `json:"color"`
}

type llmOutput struct {
	Kind    string      `json:"kind"`
	Actions []llmAction `json:"actions"`
	Reply   string      `json:"reply"`
}

// parseActions: JSON estricto (campos desconocidos: error, nada detrás del
// objeto) y cada acción validada. Una sola acción mala invalida todas: si el
// modelo se equivocó en una, las otras tampoco son de fiar.
func parseActions(content, text string) (acts []Action, kind, reply string, err error) {
	dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(content)))
	dec.DisallowUnknownFields()
	var out llmOutput
	if err = dec.Decode(&out); err != nil {
		return nil, "", "", err
	}
	if dec.More() {
		return nil, "", "", errors.New("trailing data after the JSON object")
	}
	if out.Actions == nil {
		return nil, "", "", errors.New("missing actions")
	}
	// kind obliga al modelo a clasificar la frase ANTES de proponer acciones
	// (genera en el orden del esquema). Y es una comprobación cruzada: «no es
	// para esta habitación» con acciones es una respuesta incoherente.
	switch out.Kind {
	case KindCommand, KindSituation:
		if len(out.Actions) == 0 {
			return nil, "", "", fmt.Errorf("kind %s without actions", out.Kind)
		}
	case KindOther:
		if len(out.Actions) > 0 {
			return nil, "", "", errors.New("kind other with actions")
		}
	default:
		return nil, "", "", fmt.Errorf("unknown kind %q", out.Kind)
	}
	if len(out.Actions) > maxActions {
		return nil, "", "", fmt.Errorf("%d actions (max %d)", len(out.Actions), maxActions)
	}
	named := mentioned(text)
	_, hasValue := ParseValue(text)
	acts = make([]Action, 0, len(out.Actions))
	for i, a := range out.Actions {
		act, verr := validAction(a)
		if verr != nil {
			return nil, "", "", fmt.Errorf("action %d: %w", i, verr)
		}
		// Una zona que la frase no nombra es invención del modelo («me voy a
		// dormir» → solo el dormitorio): se quita y la acción vale para toda
		// la habitación, que es lo que se dijo.
		if act.Slots.Area != "" && !named[act.Slots.Area] {
			act.Slots.Area = ""
		}
		// Tampoco puede inventar un color ni un número que la frase no dice
		// («cambia el color de la luz» → azul): mejor preguntar. Medido en
		// MASSIVE, era la mitad de sus acciones equivocadas.
		if act.Slots.Color != "" && !named["color:"+act.Slots.Color] {
			return nil, "", "", fmt.Errorf("color %s is not in the request", act.Slots.Color)
		}
		if act.Slots.HasValue && !hasValue {
			return nil, "", "", fmt.Errorf("value %v is not in the request", act.Slots.Value)
		}
		// Abrir la puerta o desarmar la alarma solo si la frase lo dice: nunca
		// por una lectura «indirecta» del modelo.
		if (act.Intent == "unlock" && !named[DevLock]) || (act.Intent == "alarm_disarm" && !named[DevAlarm]) {
			return nil, "", "", fmt.Errorf("%s without naming the device", act.Intent)
		}
		acts = append(acts, act)
	}
	return acts, out.Kind, cleanReply(out.Reply), nil
}

// hasCommandVerb: ¿hay un verbo de orden (imperativo) en la frase?
func hasCommandVerb(text string) bool {
	for _, t := range slots.Tokenize(text) {
		if commandVerbs[t.Norm] {
			return true
		}
	}
	return false
}

// mentioned: zonas, dispositivos y colores («color:<canónico>») canónicos que
// la frase nombra.
func mentioned(text string) map[string]bool {
	out := map[string]bool{}
	for _, sp := range FindSpans(text) {
		t := text[sp.Start:sp.End]
		switch sp.Slot {
		case SlotArea:
			if c, ok := CanonArea(t); ok {
				out[c] = true
			}
		case SlotDevice:
			if c, ok := CanonDevice(t); ok {
				out[c] = true
			}
		case SlotColor:
			if c, ok := CanonColor(t); ok {
				out["color:"+c] = true
			}
		}
	}
	// El léxico no trae plurales de colores («luces azules», «rojas»).
	for _, tk := range slots.Tokenize(text) {
		for _, f := range []string{strings.TrimSuffix(tk.Norm, "es"), strings.TrimSuffix(tk.Norm, "s")} {
			if c, ok := colorIx.full[f]; ok {
				out["color:"+c] = true
			}
		}
	}
	return out
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func validAction(a llmAction) (Action, error) {
	it := Intent(a.Intent)
	if it == nil || a.Intent == OutOfScope {
		return Action{}, fmt.Errorf("unknown intent %q", a.Intent)
	}
	s := Slots{Device: str(a.Device), Area: str(a.Area), Color: str(a.Color)}
	if s.Device != "" && !validDevice[s.Device] {
		return Action{}, fmt.Errorf("unknown device %q", s.Device)
	}
	if s.Device != "" && !deviceFits(a.Intent, s.Device) {
		return Action{}, fmt.Errorf("device %q does not fit %s", s.Device, a.Intent)
	}
	if s.Area != "" && !validArea[s.Area] {
		return Action{}, fmt.Errorf("unknown area %q", s.Area)
	}
	if s.Color != "" && !validColor[s.Color] {
		return Action{}, fmt.Errorf("unknown color %q", s.Color)
	}
	if a.Value != nil {
		v := *a.Value
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return Action{}, errors.New("value is not a number")
		}
		s.Value, s.HasValue = v, true
	}
	s = Resolve(a.Intent, s)
	if s.HasValue {
		lo, hi := 0.0, 100.0
		if s.Unit == UnitCelsius {
			lo, hi = 5, 35
		}
		if s.Value < lo || s.Value > hi {
			return Action{}, fmt.Errorf("value %v out of range [%v, %v]", s.Value, lo, hi)
		}
	}
	if !Complete(a.Intent, s) {
		return Action{}, fmt.Errorf("%s is missing its value or color", a.Intent)
	}
	return Action{Intent: a.Intent, Slots: s}, nil
}

// deviceFits: el dispositivo tiene que poder hacer la intención. Encender o
// apagar vale para lo que tiene interruptor; lo multimedia, para tele y
// altavoz; el resto, solo su dispositivo.
func deviceFits(intent, device string) bool {
	switch intent {
	case "turn_on", "turn_off":
		return device != DevLock && device != DevAlarm && device != DevBlinds
	case "media_pause", "media_resume", "media_next", "media_previous",
		"volume_up", "volume_down", "volume_set", "volume_mute", "volume_unmute":
		return device == DevTV || device == DevSpeaker
	}
	return device == Intent(intent).Device
}

// cleanReply quita caracteres de control y recorta: la frase va a una página
// web y a un sintetizador de voz.
func cleanReply(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.TrimSpace(s))
	return clip(s, maxReplyRune)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

var (
	validDevice = keySet(deviceForms)
	validArea   = keySet(areaForms)
	validColor  = keySet(colorForms)
)

func keySet(m map[string][]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// LLMSchema es el esquema JSON de la respuesta. Se escribe a mano (y no con
// un map) porque el orden de las propiedades importa: llama-server genera en
// ese orden, y el modelo decide mejor la intención antes que la zona.
var LLMSchema = json.RawMessage(buildSchema())

func buildSchema() string {
	var intents []string
	for _, it := range Intents {
		if it.Name != OutOfScope {
			intents = append(intents, it.Name)
		}
	}
	enum := func(xs []string) string {
		b, _ := json.Marshal(xs)
		// null al final: «no lo dice» es una respuesta válida.
		return `{"enum":` + strings.TrimSuffix(string(b), "]") + `,null]}`
	}
	intentsJSON, _ := json.Marshal(intents)
	// Orden: kind, reply, actions. Lo que se genera antes condiciona lo de
	// después: medido en las frases de reto, con la frase de vuelta al final el
	// modelo escribía «bajo el volumen» junto a volume_up; escrita antes, la
	// acción la sigue.
	return `{"type":"object","additionalProperties":false,"required":["kind","reply","actions"],"properties":{` +
		`"kind":{"enum":["` + KindCommand + `","` + KindSituation + `","` + KindOther + `"]},` +
		`"reply":{"type":"string","maxLength":160},` +
		`"actions":{"type":"array","maxItems":` + fmt.Sprint(maxActions) + `,"items":{"type":"object","additionalProperties":false,` +
		`"required":["intent","device","area","value","color"],"properties":{` +
		`"intent":{"enum":` + string(intentsJSON) + `},` +
		`"device":` + enum(sortedKeys(deviceForms)) + `,` +
		`"area":` + enum(sortedKeys(areaForms)) + `,` +
		`"value":{"type":["number","null"]},` +
		`"color":` + enum(sortedKeys(colorForms)) + `}}}}}`
}

// LLMSystemPrompt es el mensaje de sistema: la taxonomía, el léxico canónico,
// las reglas y unos pocos ejemplos. Es fijo a propósito (ver el comentario del
// principio: caché de prefijo). Los ejemplos NO son frases de reto.
var LLMSystemPrompt = buildSystemPrompt()

func buildSystemPrompt() string {
	var b strings.Builder
	b.WriteString("You control the devices of a smart home from spoken requests in Spanish or English. " +
		"Turn each request into a JSON object: its kind, a short reply saying what you will do, and the list of actions that does exactly that.\n\nIntents (exact names):\n")
	for _, it := range Intents {
		if it.Name != OutOfScope {
			fmt.Fprintf(&b, "- %s: %s\n", it.Name, it.Desc)
		}
	}
	fmt.Fprintf(&b, "\nDevices: %s.\nAreas: %s.\nColors: %s.\n", strings.Join(sortedKeys(deviceForms), ", "),
		strings.Join(sortedKeys(areaForms), ", "), strings.Join(sortedKeys(colorForms), ", "))
	b.WriteString(`
Rules:
0. kind: "command" for direct orders, "situation" when the request only describes how something is or feels, "other" when nothing in this home fits (then actions is []).
1. One action per order. Two orders in one request ("... and ...", "... y ...") give two actions.
2. Indirect requests describe a situation; act on it: cold -> temperature_up; hot -> temperature_down; too dark or cannot see -> turn_on light; too bright -> brightness_down; sun or glare -> cover_close; cannot hear -> volume_up; too loud -> volume_down; going to sleep -> turn_off light; leaving home -> alarm_arm.
3. value is a number in the unit of the intent (% or degrees C). "half" is 50, "max" or "full" is 100, "a quarter" is 25. set_temperature, set_brightness, cover_set_position, volume_set and fan_set_speed need a number; without a number use the _up or _down intent with value null.
4. Fill device and area only when the request names them; otherwise null. The room state only helps to understand the request.
5. Anything the room cannot do is kind "other" with "actions": []: wake-up alarms, timers and reminders (alarm_arm is only the security alarm), playing a song or genre, weather, shopping, questions about a device, devices not listed (coffee maker, vacuum, garage door). Never invent actions.
6. reply is one short sentence in the language of the request; the actions must do exactly what it says.

Examples:
Request: pon la cocina en verde y abre las persianas
{"kind":"command","reply":"Cocina en verde y persianas abiertas.","actions":[{"intent":"set_color","device":null,"area":"kitchen","value":null,"color":"green"},{"intent":"cover_open","device":"blinds","area":null,"value":null,"color":null}]}
Request: tengo un poco de calor
{"kind":"situation","reply":"Bajo un poco la temperatura.","actions":[{"intent":"temperature_down","device":null,"area":null,"value":null,"color":null}]}
Request: it's pitch black in the hallway
{"kind":"situation","reply":"Hallway light on.","actions":[{"intent":"turn_on","device":"light","area":"hallway","value":null,"color":null}]}
Request: close the blinds and set the fan to 40
{"kind":"command","reply":"Blinds closed, fan at 40%.","actions":[{"intent":"cover_close","device":"blinds","area":null,"value":null,"color":null},{"intent":"fan_set_speed","device":"fan","area":null,"value":40,"color":null}]}
Request: leave the office blinds a quarter open
{"kind":"command","reply":"Office blinds at 25%.","actions":[{"intent":"cover_set_position","device":"blinds","area":"office","value":25,"color":null}]}
Request: sube un poco la tele
{"kind":"command","reply":"Subo el volumen de la tele.","actions":[{"intent":"volume_up","device":"tv","area":null,"value":null,"color":null}]}
Request: recuérdame llamar a mamá
{"kind":"other","reply":"No puedo poner recordatorios desde la casa.","actions":[]}
Request: tell me a joke
{"kind":"other","reply":"I can only control the devices of this home.","actions":[]}
`)
	return b.String()
}

// validatorVersion cambia cuando cambian las reglas de validación o el veto:
// deciden tanto como el prompt, así que entran en PromptID.
const validatorVersion = "2"

// PromptID identifica el prompt, el esquema y la validación: una evaluación
// solo respalda la capa 4 con los mismos, y con el mismo modelo.
func PromptID() string {
	h := sha256.Sum256([]byte(LLMSystemPrompt + "\x00" + string(LLMSchema) + "\x00" + validatorVersion))
	return hex.EncodeToString(h[:6])
}
