package domotica

import (
	"sort"
	"strings"

	"github.com/juan52878911/kindling/pkg/chispa/slots"
)

// Léxico de la habitación de demo: nombres canónicos de zonas, dispositivos y
// colores con sus sinónimos en español e inglés. Es la normalización de
// valores (lo que convierte «las luces del salón» en device=light,
// area=living_room) y, aplanado a tokens sueltos, el léxico que ve el
// etiquetador como característica. Si se toca, cambia el hash del .chispas y hay
// que reentrenar: a propósito, para no mezclar léxicos en silencio.

// Dispositivos canónicos de la habitación.
const (
	DevLight      = "light"
	DevThermostat = "thermostat"
	DevBlinds     = "blinds"
	DevTV         = "tv"
	DevLock       = "lock"
	DevAlarm      = "alarm"
	DevFan        = "fan"
	DevSpeaker    = "speaker"
	DevPlug       = "plug"
)

var deviceForms = map[string][]string{
	DevLight: {"luz", "luces", "lámpara", "lámparas", "lampara", "bombilla", "bombillas", "bombillo", "foco", "focos",
		"plafón", "flexo", "lamparita", "tira led", "tira de led", "iluminación", "ampolleta",
		"light", "lights", "lamp", "lamps", "bulb", "bulbs", "lighting", "led strip", "hue", "ceiling light", "desk lamp", "floor lamp"},
	DevThermostat: {"termostato", "calefacción", "calefaccion", "climatización", "aire acondicionado", "aire", "radiador", "estufa",
		"thermostat", "heating", "heater", "heat", "air conditioning", "air conditioner", "ac", "a c", "climate", "hvac"},
	DevBlinds: {"persiana", "persianas", "cortina", "cortinas", "estor", "estores", "toldo", "veneciana", "venecianas", "contraventana", "contraventanas",
		"blind", "blinds", "shade", "shades", "curtain", "curtains", "shutter", "shutters", "awning"},
	DevTV: {"tele", "televisión", "television", "televisor", "tv", "telly", "tele del salón"},
	DevLock: {"cerradura", "puerta", "puerta principal", "puerta de entrada", "pestillo", "cerrojo", "candado",
		"lock", "door", "front door", "door lock", "deadbolt", "back door", "entrance door"},
	DevAlarm:   {"alarma", "sistema de seguridad", "alarma de seguridad", "alarm", "security system", "security alarm", "burglar alarm"},
	DevFan:     {"ventilador", "ventiladores", "abanico", "abanicos", "fan", "fans", "ceiling fan"},
	DevSpeaker: {"altavoz", "altavoces", "bocina", "bocinas", "parlante", "parlantes", "equipo de música", "speaker", "speakers", "sound system", "stereo", "sonos"},
	DevPlug: {"enchufe", "enchufes", "toma de corriente", "regleta", "clavija", "wemo",
		"plug", "plugs", "socket", "sockets", "outlet", "outlets", "smart plug", "power strip"},
}

var areaForms = map[string][]string{
	"living_room": {"salón", "salon", "sala", "sala de estar", "living", "cuarto de estar", "living room", "lounge", "family room", "front room", "sitting room"},
	"kitchen":     {"cocina", "kitchen"},
	"bedroom":     {"dormitorio", "recámara", "alcoba", "dormitorio principal", "bedroom", "master bedroom", "bed room"},
	"room":        {"habitación", "habitacion", "cuarto", "pieza", "room"},
	"bathroom":    {"baño", "aseo", "servicio", "bathroom", "restroom", "washroom", "toilet"},
	"office":      {"oficina", "despacho", "estudio", "office", "study"},
	"dining_room": {"comedor", "dining room"},
	"hallway":     {"pasillo", "recibidor", "entrada", "hall", "hallway", "corridor", "entrance", "entryway"},
	"garage":      {"garaje", "garage"},
	"garden":      {"jardín", "jardin", "terraza", "patio", "balcón", "garden", "yard", "backyard", "balcony", "terrace"},
	"house":       {"casa", "toda la casa", "hogar", "piso", "apartamento", "house", "home", "whole house", "apartment", "flat"},
	"kids_room":   {"cuarto de los niños", "habitación de los niños", "kids room", "nursery"},
	"guest_room":  {"habitación de invitados", "cuarto de invitados", "guest room"},
}

var colorForms = map[string][]string{
	"white":  {"blanco", "blanca", "white"},
	"black":  {"negro", "black"},
	"red":    {"rojo", "roja", "red"},
	"orange": {"naranja", "anaranjado", "orange"},
	"yellow": {"amarillo", "amarilla", "yellow"},
	"green":  {"verde", "green"},
	"blue":   {"azul", "blue"},
	"purple": {"morado", "morada", "lila", "púrpura", "violeta", "purple", "violet", "lilac"},
	"brown":  {"marrón", "café", "brown"},
	"pink":   {"rosa", "rosado", "rosada", "pink"},
	"cyan":   {"cian", "celeste", "turquesa", "cyan", "turquoise", "light blue"},
	"warm":   {"cálida", "cálido", "calida", "warm", "warm white", "blanco cálido"},
	"cool":   {"fría", "frío", "cool", "cold", "cool white", "blanco frío"},
}

// Palabras que se quitan de los bordes de un valor antes de normalizarlo:
// artículos, preposiciones y posesivos que MASSIVE a veces incluye en el hueco.
var edgeWords = setOf("el", "la", "los", "las", "lo", "del", "de", "en", "al", "a", "mi", "mis", "nuestro", "nuestra", "un", "una",
	"este", "esta", "the", "my", "our", "in", "of", "on", "to", "at", "this", "that", "a", "an", "for", "by", "hasta", "sobre", "up", "down")

func setOf(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// Norm pliega un texto como el tokenizador (minúsculas, sin acentos ni
// puntuación) y une los tokens con un espacio.
func Norm(s string) string {
	toks := slots.Tokenize(s)
	parts := make([]string, len(toks))
	for i, t := range toks {
		parts[i] = t.Norm
	}
	return strings.Join(parts, " ")
}

type index struct {
	full   map[string]string // forma normalizada completa → canónico
	maxLen int               // tokens de la forma más larga
}

func buildIndex(forms map[string][]string) index {
	ix := index{full: map[string]string{}}
	canons := make([]string, 0, len(forms))
	for c := range forms {
		canons = append(canons, c)
	}
	sort.Strings(canons) // determinista si dos canónicos comparten forma
	for _, c := range canons {
		for _, f := range forms[c] {
			n := Norm(f)
			if _, dup := ix.full[n]; !dup {
				ix.full[n] = c
			}
			if k := len(strings.Fields(n)); k > ix.maxLen {
				ix.maxLen = k
			}
		}
	}
	return ix
}

var (
	deviceIx = buildIndex(deviceForms)
	areaIx   = buildIndex(areaForms)
	colorIx  = buildIndex(colorForms)
)

// trimEdges quita artículos y preposiciones de los extremos.
func trimEdges(toks []string) []string {
	for len(toks) > 0 && edgeWords[toks[0]] {
		toks = toks[1:]
	}
	for len(toks) > 0 && edgeWords[toks[len(toks)-1]] {
		toks = toks[:len(toks)-1]
	}
	return toks
}

// canon busca el canónico de un hueco: la forma completa (sin bordes) o, si
// no, la subsecuencia conocida más larga («lámpara del techo» → light). Si
// nada encaja devuelve el texto normalizado, para que dos formas iguales
// sigan comparando iguales. known dice si se reconoció.
func canon(ix index, text string) (value string, known bool) {
	toks := trimEdges(strings.Fields(Norm(text)))
	if len(toks) == 0 {
		return "", false
	}
	if c, ok := ix.full[strings.Join(toks, " ")]; ok {
		return c, true
	}
	for n := min(ix.maxLen, len(toks)); n >= 1; n-- {
		for i := 0; i+n <= len(toks); i++ {
			if c, ok := ix.full[strings.Join(toks[i:i+n], " ")]; ok {
				return c, true
			}
		}
	}
	return strings.Join(toks, " "), false
}

// CanonDevice, CanonArea y CanonColor normalizan el texto de un hueco.
func CanonDevice(text string) (string, bool) { return canon(deviceIx, text) }

// CanonArea: ver CanonDevice.
func CanonArea(text string) (string, bool) { return canon(areaIx, text) }

// CanonColor: ver CanonDevice.
func CanonColor(text string) (string, bool) { return canon(colorIx, text) }

// Lexicon es el léxico aplanado a tokens para el etiquetador: cada token de
// una forma de una sola palabra, con su clase. Las formas de varias palabras
// no se trocean: «de» o «room» sueltos no son una zona.
func Lexicon() map[string]string {
	lex := map[string]string{}
	add := func(forms map[string][]string, class string) {
		for _, fs := range forms {
			for _, f := range fs {
				if n := Norm(f); n != "" && !strings.Contains(n, " ") {
					if _, dup := lex[n]; !dup {
						lex[n] = class
					}
				}
			}
		}
	}
	add(areaForms, "area")
	add(deviceForms, "device")
	add(colorForms, "color")
	for w := range numberWordsES {
		lex[w] = "num"
	}
	for w := range numberWordsEN {
		lex[w] = "num"
	}
	for _, w := range []string{"%", "por", "ciento", "porciento", "percent", "grados", "grado", "degrees", "degree", "°", "celsius", "fahrenheit", "centigrados"} {
		lex[Norm(w)] = "unit"
	}
	for _, w := range []string{"maximo", "maxima", "max", "maximum", "minimo", "minima", "min", "minimum", "tope", "full", "mitad", "half"} {
		lex[w] = "num"
	}
	return lex
}

// DetectLang distingue español de inglés contando palabras funcionales. Es
// tosco a propósito: para órdenes de domótica basta y no cuesta nada. Ante la
// duda (ninguna pista), español: el idioma principal de la demo.
func DetectLang(text string) string {
	var tk slots.Tokenizer
	tk.Run(text)
	es, en := 0, 0
	for i := 0; i < tk.Len(); i++ {
		w := string(tk.Tok(i))
		if esWords[w] {
			es++
		}
		if enWords[w] {
			en++
		}
	}
	if en > es {
		return "en"
	}
	return "es"
}

var esWords = setOf("el", "la", "los", "las", "de", "del", "en", "que", "por", "pon", "enciende", "apaga", "sube", "baja", "luz", "luces",
	"y", "al", "un", "una", "favor", "esta", "hace", "mi", "abre", "cierra", "puedes", "temperatura", "volumen", "persianas", "alarma",
	"quiero", "con", "para", "se", "es", "me", "mas", "menos", "grados", "salon", "cocina", "dormitorio", "activa", "desactiva")
var enWords = setOf("the", "of", "in", "on", "off", "turn", "set", "please", "to", "and", "my", "is", "it", "lights", "light", "up",
	"down", "open", "close", "what", "can", "you", "switch", "make", "volume", "temperature", "blinds", "alarm", "lock", "unlock",
	"i", "want", "with", "for", "be", "me", "more", "less", "degrees", "room", "kitchen", "bedroom", "living", "start", "stop")

// FindSpans marca en text las menciones del léxico (zonas, dispositivos,
// colores) de izquierda a derecha y de la más larga a la más corta, sin
// solapes. kinds elige cuáles («area», «device», «color»); vacío = todas.
// Lo usan la línea base de reglas y el generador de datos, que etiqueta con
// ella los nombres de dispositivo que las fuentes dejan sin anotar.
func FindSpans(text string, kinds ...string) []slots.Span {
	want := func(k string) bool {
		if len(kinds) == 0 {
			return true
		}
		for _, x := range kinds {
			if x == k {
				return true
			}
		}
		return false
	}
	type lx struct {
		ix   index
		slot string
	}
	var ixs []lx
	for _, c := range []lx{{areaIx, SlotArea}, {deviceIx, SlotDevice}, {colorIx, SlotColor}} {
		if want(c.slot) {
			ixs = append(ixs, c)
		}
	}
	toks := slots.Tokenize(text)
	var out []slots.Span
	for i := 0; i < len(toks); {
		bestK, bestSlot := 0, ""
		for _, c := range ixs {
			for k := min(c.ix.maxLen, len(toks)-i); k > bestK; k-- {
				parts := make([]string, k)
				for j := range parts {
					parts[j] = toks[i+j].Norm
				}
				if _, ok := c.ix.full[strings.Join(parts, " ")]; ok {
					bestK, bestSlot = k, c.slot
					break
				}
			}
		}
		if bestK == 0 {
			i++
			continue
		}
		out = append(out, slots.Span{Slot: bestSlot, Start: toks[i].Start, End: toks[i+bestK-1].End,
			Text: text[toks[i].Start:toks[i+bestK-1].End]})
		i += bestK
	}
	return out
}
