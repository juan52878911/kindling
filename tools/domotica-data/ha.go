package main

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/juan52878911/kindling/pkg/domotica"
	"github.com/juan52878911/kindling/pkg/jev/slots"
)

// Expansión de home-assistant/intents. Cada plantilla es una familia: todas
// sus frases caen en el mismo reparto (ver assignSplits). Las listas
// que dependen de la casa del usuario ({area}, {name}) se rellenan con las
// zonas y los dispositivos de la habitación de demo; las plantillas con
// {floor} o con listas que el repositorio no define se omiten y se cuentan.

// Frases por plantilla (flags -ha-k y -ha-k-oos de build). Acotado y
// determinista: con la misma semilla salen las mismas frases.
var (
	perTemplateInScope = 30 // de una intención de la habitación
	perTemplateOOS     = 4  // de una fuera de ámbito (temporizadores, listas…)
)

// Zonas con las que se rellena {area}.
var haAreas = map[string][]string{
	"es": {"salón", "cocina", "dormitorio", "baño", "oficina", "comedor", "pasillo", "habitación", "despacho",
		"garaje", "jardín", "terraza", "sala de estar", "cuarto de invitados", "habitación de los niños"},
	"en": {"living room", "kitchen", "bedroom", "bathroom", "office", "dining room", "hallway", "garage",
		"garden", "study", "guest room", "family room", "kids room"},
}

type haName struct{ text, domain, device string }

// Nombres de dispositivo con los que se rellena {name}, por dominio de Home
// Assistant. Ninguno contiene una zona: el nombre entero es el hueco device.
var haNames = map[string][]haName{
	"es": {
		{"lámpara", "light", "light"}, {"lámpara del techo", "light", "light"}, {"luz del escritorio", "light", "light"},
		{"lámpara de pie", "light", "light"}, {"tira led", "light", "light"}, {"flexo", "light", "light"},
		{"ventilador", "fan", "fan"}, {"ventilador de techo", "fan", "fan"},
		{"tele", "media_player", "tv"}, {"televisión", "media_player", "tv"}, {"televisor", "media_player", "tv"},
		{"altavoz", "media_player", "speaker"}, {"altavoz inteligente", "media_player", "speaker"},
		{"enchufe", "switch", "plug"}, {"enchufe inteligente", "switch", "plug"}, {"regleta", "switch", "plug"},
		{"termostato", "climate", "thermostat"}, {"calefacción", "climate", "thermostat"}, {"aire acondicionado", "climate", "thermostat"},
		{"persiana", "cover", "blinds"}, {"persianas", "cover", "blinds"}, {"cortina", "cover", "blinds"}, {"estor", "cover", "blinds"},
		{"puerta principal", "lock", "lock"}, {"cerradura", "lock", "lock"}, {"puerta de la calle", "lock", "lock"},
		{"robot aspirador", "vacuum", ""}, {"lista de la compra", "todo", ""}, {"cortacésped", "lawn_mower", ""},
		{"sirena", "siren", ""}, {"riego", "valve", ""}, {"llave de paso", "valve", ""},
	},
	"en": {
		{"lamp", "light", "light"}, {"ceiling light", "light", "light"}, {"desk lamp", "light", "light"},
		{"floor lamp", "light", "light"}, {"led strip", "light", "light"}, {"reading light", "light", "light"},
		{"fan", "fan", "fan"}, {"ceiling fan", "fan", "fan"},
		{"tv", "media_player", "tv"}, {"television", "media_player", "tv"},
		{"speaker", "media_player", "speaker"}, {"smart speaker", "media_player", "speaker"}, {"sound system", "media_player", "speaker"},
		{"plug", "switch", "plug"}, {"smart plug", "switch", "plug"}, {"power strip", "switch", "plug"},
		{"thermostat", "climate", "thermostat"}, {"heater", "climate", "thermostat"}, {"air conditioner", "climate", "thermostat"},
		{"blinds", "cover", "blinds"}, {"curtains", "cover", "blinds"}, {"shades", "cover", "blinds"}, {"roller blind", "cover", "blinds"},
		{"front door", "lock", "lock"}, {"door lock", "lock", "lock"}, {"back door", "lock", "lock"},
		{"robot vacuum", "vacuum", ""}, {"shopping list", "todo", ""}, {"lawn mower", "lawn_mower", ""},
		{"siren", "siren", ""}, {"sprinklers", "valve", ""}, {"water valve", "valve", ""},
	},
}

// Dominios que HA considera «encendibles» (name_domains: default).
var onOffDomains = []string{"light", "fan", "switch", "media_player", "climate"}

// Valores para los comodines. Ninguno es una orden de la habitación: un
// comodín mal elegido metería una orden válida en una frase fuera de ámbito.
var haFillers = map[string]map[string][]string{
	"es": {
		"search_query":       {"música clásica", "las noticias", "un podcast de ciencia", "jazz"},
		"message":            {"la cena está lista", "salgo en cinco minutos"},
		"shopping_list_item": {"leche", "pan", "huevos", "manzanas"},
		"todo_list_item":     {"llamar al médico", "pagar el alquiler"},
		"timer_name":         {"pasta", "horno", "lavadora"},
		"timer_command":      {"riega las plantas", "avísame"},
		"zone":               {"trabajo", "el gimnasio", "el colegio"},
	},
	"en": {
		"search_query":       {"classical music", "the news", "a science podcast", "jazz"},
		"message":            {"dinner is ready", "leaving in five minutes"},
		"shopping_list_item": {"milk", "bread", "eggs", "apples"},
		"todo_list_item":     {"call the doctor", "pay the rent"},
		"timer_name":         {"pasta", "oven", "laundry"},
		"timer_command":      {"water the plants", "remind me"},
		"zone":               {"work", "the gym", "school"},
	},
}

// Cortinas y similares son «blinds»; puertas, verjas y ventanas motorizadas
// no están en la habitación y esas frases se omiten.
var blindLike = map[string]bool{"blind": true, "curtain": true, "shade": true, "shutter": true, "awning": true}

type haBlock struct {
	intent, family string
	sentences      []string
	fixed          map[string]string
	inferred       string
	nameDomains    []string
}

type haStats struct {
	files, blocks, templates, skippedTemplates, skippedFiles, rows, dropped int
	skipReasons                                                             map[string]int
}

func (s *haStats) skip(reason string) {
	if s.skipReasons == nil {
		s.skipReasons = map[string]int{}
	}
	s.skipReasons[reason]++
}

// haGrammar reúne reglas y listas de un idioma del repositorio.
func haGrammar(files map[string][]byte, lang string) (*domotica.Grammar, error) {
	g := &domotica.Grammar{Lang: lang, Rules: map[string]string{}, Lists: map[string]*domotica.List{}}
	names := sortedKeys(files)
	for _, name := range names {
		dir := path.Dir(name)
		switch {
		case dir == "rules/"+lang:
			docs, err := parseYAMLDocs(string(files[name]))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			for _, d := range docs {
				for k, v := range ymap(ymap(d)["expansion_rules"]) {
					g.Rules[k] = ystr(v)
				}
			}
		case dir == "lists" || dir == "lists/"+lang:
			docs, err := parseYAMLDocs(string(files[name]))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			for _, d := range docs {
				for k, v := range ymap(ymap(d)["lists"]) {
					l, err := parseList(v)
					if err != nil {
						return nil, fmt.Errorf("%s: list %s: %w", name, k, err)
					}
					g.Lists[k] = l
				}
			}
		}
	}
	var areas []domotica.ListValue
	for _, a := range haAreas[lang] {
		areas = append(areas, domotica.ListValue{In: a, Out: a})
	}
	g.Lists["area"] = &domotica.List{Values: areas}
	fill := haFillers[lang]
	g.Filler = func(list string) []string { return fill[list] }
	return g, nil
}

func parseList(v any) (*domotica.List, error) {
	m := ymap(v)
	if m == nil {
		return nil, errors.New("not a mapping")
	}
	if ystr(m["wildcard"]) == "true" {
		return &domotica.List{Wildcard: true}, nil
	}
	if r := ymap(m["range"]); r != nil {
		from, err1 := strconv.Atoi(ystr(r["from"]))
		to, err2 := strconv.Atoi(ystr(r["to"]))
		if err1 != nil || err2 != nil || to < from || to-from > 100000 {
			return nil, errors.New("bad range")
		}
		return &domotica.List{Range: true, From: from, To: to, Halves: ystr(r["fractions"]) == "halves"}, nil
	}
	l := &domotica.List{}
	for _, it := range ylist(m["values"]) {
		switch x := it.(type) {
		case string:
			l.Values = append(l.Values, domotica.ListValue{In: x, Out: x})
		case map[string]any:
			in, out := ystr(x["in"]), ystr(x["out"])
			if in == "" {
				continue
			}
			if out == "" {
				out = in
			}
			l.Values = append(l.Values, domotica.ListValue{In: in, Out: out})
		}
	}
	return l, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func haBlocks(files map[string][]byte, lang string, st *haStats) []haBlock {
	var out []haBlock
	for _, name := range sortedKeys(files) {
		if !strings.HasPrefix(name, "sentences/"+lang+"/") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		parts := strings.Split(name, "/")
		if len(parts) != 4 { // sentences/<lang>/<Intent>/<fichero>.yaml
			continue
		}
		intent := parts[2]
		docs, err := parseYAMLDocs(string(files[name]))
		if err != nil {
			st.skippedFiles++
			st.skip("yaml: " + err.Error())
			continue
		}
		st.files++
		for _, d := range docs {
			for i, it := range ylist(ymap(d)["data"]) {
				m := ymap(it)
				b := haBlock{intent: intent, family: fmt.Sprintf("ha/%s/%s/%s#%d", lang, intent, parts[3], i),
					fixed: map[string]string{}, inferred: ystr(m["inferred_domain"])}
				for _, s := range ylist(m["sentences"]) {
					if t := ystr(s); t != "" {
						b.sentences = append(b.sentences, t)
					}
				}
				for k, v := range ymap(m["slots"]) {
					b.fixed[k] = ystr(v)
				}
				for _, v := range ylist(m["name_domains"]) {
					b.nameDomains = append(b.nameDomains, ystr(v))
				}
				if len(b.sentences) > 0 {
					out = append(out, b)
					st.blocks++
				}
			}
		}
	}
	return out
}

// nameList: los nombres de dispositivo que admite el bloque.
func nameList(lang string, b haBlock) *domotica.List {
	doms := b.nameDomains
	if len(doms) == 0 || (len(doms) == 1 && doms[0] == "default") {
		doms = onOffDomains
	}
	allowed := map[string]bool{}
	for _, d := range doms {
		allowed[d] = true
	}
	l := &domotica.List{}
	for _, n := range haNames[lang] {
		if allowed[n.domain] {
			l.Values = append(l.Values, domotica.ListValue{In: n.text, Out: n.domain + "|" + n.device})
		}
	}
	if len(l.Values) == 0 { // dominios que no tenemos: nombres genéricos fuera de ámbito
		for _, n := range haNames[lang] {
			if n.device == "" {
				l.Values = append(l.Values, domotica.ListValue{In: n.text, Out: n.domain + "|"})
			}
		}
	}
	return l
}

var turnMap = map[string]struct{ on, off, device string }{
	"light":        {"turn_on", "turn_off", domotica.DevLight},
	"fan":          {"turn_on", "turn_off", domotica.DevFan},
	"switch":       {"turn_on", "turn_off", domotica.DevPlug},
	"media_player": {"turn_on", "turn_off", domotica.DevTV},
	"climate":      {"turn_on", "turn_off", domotica.DevThermostat},
	"cover":        {"cover_open", "cover_close", domotica.DevBlinds},
	"lock":         {"lock", "unlock", domotica.DevLock},
}

// mapHA traduce una frase expandida a la taxonomía. ok=false: se omite (una
// puerta de garaje, una válvula de la que no sabemos si es persiana…).
func mapHA(b haBlock, caps map[string]domotica.Capture) (intent, device string, ok bool) {
	nameDom, nameDev := "", ""
	if c, has := caps["name"]; has {
		nameDom, nameDev, _ = strings.Cut(c.Out, "|")
	}
	dom := b.fixed["domain"]
	if dom == "" {
		dom = b.inferred
	}
	if dom == "" {
		dom = nameDom
	}
	dc := b.fixed["device_class"]
	if c, has := caps["device_class"]; has {
		dc = c.Out
	}
	if dc != "" && !blindLike[dc] {
		return "", "", false
	}
	if dc != "" {
		dom = "cover"
	}
	switch b.intent {
	case "HassTurnOn", "HassTurnOff":
		t, known := turnMap[dom]
		if !known {
			if dom == "" {
				return "", "", false
			}
			return domotica.OutOfScope, "", true // escenas, scripts, válvulas, aspiradoras…
		}
		device = t.device
		if nameDev != "" {
			device = nameDev
		}
		if b.intent == "HassTurnOn" {
			return t.on, device, true
		}
		return t.off, device, true
	case "HassLightSet":
		if _, has := caps["brightness"]; has {
			return "set_brightness", domotica.DevLight, true
		}
		if _, has := caps["color"]; has {
			return "set_color", domotica.DevLight, true
		}
		return "", "", false // temperatura de color: no está en la taxonomía
	case "HassClimateSetTemperature":
		return "set_temperature", domotica.DevThermostat, true
	case "HassClimateGetTemperature":
		return "get_temperature", domotica.DevThermostat, true
	case "HassSetPosition":
		if dom != "" && dom != "cover" {
			return "", "", false
		}
		return "cover_set_position", domotica.DevBlinds, true
	case "HassMediaPause":
		return "media_pause", nameDev, true
	case "HassMediaUnpause":
		return "media_resume", nameDev, true
	case "HassMediaNext":
		return "media_next", nameDev, true
	case "HassMediaPrevious":
		return "media_previous", nameDev, true
	case "HassSetVolume":
		return "volume_set", nameDev, true
	case "HassSetVolumeRelative":
		step := b.fixed["volume_step"]
		if c, has := caps["volume_step"]; has {
			switch c.List {
			case "volume_step_up":
				step = "up"
			case "volume_step_down":
				step = "down"
			}
		}
		switch step {
		case "up":
			return "volume_up", nameDev, true
		case "down":
			return "volume_down", nameDev, true
		}
		return "", "", false
	case "HassMediaPlayerMute":
		return "volume_mute", nameDev, true
	case "HassMediaPlayerUnmute":
		return "volume_unmute", nameDev, true
	case "HassFanSetSpeed":
		return "fan_set_speed", domotica.DevFan, true
	}
	return domotica.OutOfScope, "", true
}

// Huecos de HA que son un valor numérico.
var haValueSlots = map[string]bool{"brightness": true, "temperature": true, "position": true, "volume_level": true,
	"percentage": true, "volume_step": true}

// onlyFiller: la frase no tiene nada fuera de los huecos salvo artículos
// («la lámpara»). HA la acepta como «enciende», pero sin verbo no es una orden
// que una persona diga en la demo, y enseñaría al clasificador que un nombre
// suelto significa encender.
var fillerWords = map[string]bool{"el": true, "la": true, "los": true, "las": true, "the": true, "my": true, "a": true, "an": true, "de": true, "del": true}

func onlyFiller(e domotica.Expansion) bool {
	b := []byte(e.Text)
	for _, c := range e.Captures {
		for i := c.Start; i < c.End; i++ {
			b[i] = ' '
		}
	}
	for _, t := range slots.Tokenize(string(b)) {
		if !fillerWords[t.Norm] {
			return false
		}
	}
	return true
}

func loadHA(files map[string][]byte, st *haStats) ([]domotica.Row, error) {
	var out []domotica.Row
	for _, lang := range []string{"es", "en"} {
		g, err := haGrammar(files, lang)
		if err != nil {
			return nil, err
		}
		for _, b := range haBlocks(files, lang, st) {
			g.Lists["name"] = nameList(lang, b)
			for ti, tpl := range b.sentences {
				st.templates++
				n, err := domotica.Parse(tpl)
				if err != nil {
					st.skippedTemplates++
					st.skip("parse")
					continue
				}
				if err := g.Check(n); err != nil {
					st.skippedTemplates++
					reason := "unknown list/rule"
					if strings.Contains(err.Error(), "{floor}") {
						reason = "floor"
					}
					st.skip(reason)
					continue
				}
				k := perTemplateOOS
				if probe, _, ok := mapHA(b, nil); ok && probe != domotica.OutOfScope || !ok {
					k = perTemplateInScope
				}
				rng := &domotica.Rand{S: seedOf(b.family, ti)}
				seen := map[string]bool{}
				for try := 0; try < k*6 && len(seen) < k; try++ {
					e, err := g.Sample(n, rng)
					if err != nil {
						return nil, fmt.Errorf("%s: %q: %w", b.family, tpl, err)
					}
					norm := domotica.Norm(e.Text)
					if seen[norm] || norm == "" {
						continue
					}
					seen[norm] = true
					if onlyFiller(e) {
						st.dropped++
						continue
					}
					caps := map[string]domotica.Capture{}
					for _, c := range e.Captures {
						caps[c.Slot] = c
					}
					intent, device, ok := mapHA(b, caps)
					if !ok {
						st.dropped++
						continue
					}
					// La familia es la plantilla: sus expansiones se parecen
					// demasiado para separarlas entre repartos.
					r := domotica.Row{Text: e.Text, Lang: lang, Intent: intent, Source: "ha", Family: fmt.Sprintf("%s/%d", b.family, ti)}
					if intent != domotica.OutOfScope {
						for _, c := range e.Captures {
							var slot string
							switch {
							case c.Slot == "area":
								slot = domotica.SlotArea
							case c.Slot == "name" && device != "":
								slot = domotica.SlotDevice
							case c.Slot == "device_class":
								slot = domotica.SlotDevice
							case c.Slot == "color":
								slot = domotica.SlotColor
							case haValueSlots[c.Slot]:
								slot = domotica.SlotValue
							default:
								continue
							}
							r.Spans = append(r.Spans, slots.Span{Slot: slot, Start: c.Start, End: c.End})
						}
					}
					finishRow(&r, device)
					out = append(out, r)
					st.rows++
				}
			}
		}
	}
	return out, nil
}
