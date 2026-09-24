package domotica

// Órdenes de la demo: la capa 1 las reconoce siempre, en microsegundos y sin
// modelo. Son plantillas (sintaxis en template.go) con tres listas que el
// emparejador rellena al vuelo: {area} (zonas de lexicon.go), {value} (un
// número en cifras o palabras, o «máximo») y {color}. Todo lo que no encaje
// exactamente —tras plegar mayúsculas, acentos y puntuación y quitar cortesías
// como «por favor»— pasa a la capa siguiente.

// DemoTemplate es una familia de órdenes de la demo.
type DemoTemplate struct {
	Lang     string
	Intent   string
	Device   string // dispositivo que nombra la plantilla ("" = el implícito)
	Template string
}

var demoRules = map[string]map[string]string{
	"es": {
		"art":    "(el|la|los|las)",
		"luz":    "(luz|luces|lámpara|lámparas)",
		"area":   "[(en|de|del)] [(el|la|los|las)] {area}",
		"porc":   "(%|por ciento)",
		"grados": "(grados|°)",
		"clima":  "(la temperatura|la calefacción|el termostato|el aire acondicionado|el aire)",
		"cover":  "[(el|la|los|las)] (persiana|persianas|cortina|cortinas|estor|estores)",
		"media":  "[(la tele|la televisión|la música|la película|el vídeo|la serie|la reproducción)]",
		"a":      "(a|al)",
	},
	"en": {
		"light": "(light|lights|lamp|lamps)",
		"area":  "(in|of|on) [the] {area}",
		"pct":   "(%|percent)",
		"deg":   "(degrees|°)",
		"clima": "(temperature|heating|heat|thermostat|ac|air conditioning)",
		"cover": "[the] (blinds|blind|curtains|curtain|shades|shade|shutters)",
		"media": "[the] [(tv|television|music|movie|show|video)]",
	},
}

// DemoTemplates es el conjunto de órdenes de la demo.
var DemoTemplates = []DemoTemplate{
	// Luces.
	{"es", "turn_on", DevLight, "(enciende|prende|activa|conecta) [<art>] <luz> [<area>]"},
	{"es", "turn_on", DevLight, "(enciende|prende) [<art>] {area}"},
	{"es", "turn_off", DevLight, "(apaga|desconecta|desactiva) [<art>] <luz> [<area>]"},
	{"es", "turn_off", DevLight, "apaga [<art>] {area}"},
	{"es", "set_brightness", DevLight, "(pon|ajusta|sube|baja) [<art>] <luz> [<area>] <a> {value} [<porc>]"},
	{"es", "set_brightness", DevLight, "(pon|ajusta|sube|baja) el brillo [de <art> <luz>] [<area>] <a> {value} [<porc>]"},
	{"es", "brightness_up", DevLight, "(sube|aumenta) [<art>] <luz> [<area>]"},
	{"es", "brightness_up", DevLight, "(sube|aumenta) el brillo [de <art> <luz>] [<area>]"},
	{"es", "brightness_up", DevLight, "más luz [<area>]"},
	{"es", "brightness_down", DevLight, "(baja|atenúa|reduce) [<art>] <luz> [<area>]"},
	{"es", "brightness_down", DevLight, "(baja|reduce) el brillo [de <art> <luz>] [<area>]"},
	{"es", "brightness_down", DevLight, "menos luz [<area>]"},
	{"es", "set_color", DevLight, "(pon|cambia) [<art>] <luz> [<area>] [(de color|en|a)] {color}"},
	{"es", "set_color", DevLight, "(pon|cambia) el color de [<art>] <luz> [<area>] a {color}"},
	{"es", "set_color", DevLight, "luz {color} [<area>]"},
	// Termostato.
	{"es", "turn_on", DevThermostat, "(enciende|prende|activa|conecta) (la calefacción|el termostato|el aire acondicionado|el aire) [<area>]"},
	{"es", "turn_off", DevThermostat, "(apaga|desconecta|desactiva) (la calefacción|el termostato|el aire acondicionado|el aire) [<area>]"},
	{"es", "set_temperature", DevThermostat, "(pon|ajusta|sube|baja|cambia) [<clima>] [<area>] a {value} [<grados>]"},
	{"es", "set_temperature", DevThermostat, "(quiero|pon) {value} <grados> [<area>]"},
	{"es", "temperature_up", DevThermostat, "(sube|aumenta) <clima> [<area>]"},
	{"es", "temperature_down", DevThermostat, "(baja|reduce) <clima> [<area>]"},
	{"es", "get_temperature", DevThermostat, "(qué|cuál es la) temperatura [(hace|hay)] [<area>]"},
	{"es", "get_temperature", DevThermostat, "(a cuántos grados estamos|cuántos grados hace) [<area>]"},
	// Persianas.
	{"es", "cover_open", DevBlinds, "(abre|sube) <cover> [<area>]"},
	{"es", "cover_close", DevBlinds, "(cierra|baja) <cover> [<area>]"},
	{"es", "cover_set_position", DevBlinds, "(pon|abre|sube|baja|deja) <cover> [<area>] <a> {value} [<porc>]"},
	// Puerta y alarma.
	{"es", "lock", DevLock, "(cierra|bloquea) [la] (puerta|cerradura) [con llave]"},
	{"es", "lock", DevLock, "(echa el (cerrojo|pestillo) [a la puerta]|cierra con llave [la puerta])"},
	{"es", "unlock", DevLock, "(abre|desbloquea) [la] (puerta|cerradura)"},
	{"es", "unlock", DevLock, "quita el (cerrojo|pestillo)"},
	{"es", "alarm_arm", DevAlarm, "(activa|arma|conecta|enciende|pon) la alarma"},
	{"es", "alarm_disarm", DevAlarm, "(desactiva|desarma|desconecta|apaga|quita) la alarma"},
	// Tele, ventilador, altavoz, enchufe.
	{"es", "turn_on", DevTV, "(enciende|prende|pon) [(el|la)] (tele|televisión|televisor)"},
	{"es", "turn_off", DevTV, "apaga [(el|la)] (tele|televisión|televisor)"},
	{"es", "turn_on", DevFan, "(enciende|prende|pon) [el] ventilador [<area>]"},
	{"es", "turn_off", DevFan, "apaga [el] ventilador [<area>]"},
	{"es", "fan_set_speed", DevFan, "(pon|ajusta) [la velocidad del] ventilador <a> {value} [<porc>]"},
	{"es", "turn_on", DevSpeaker, "(enciende|activa) [el] altavoz [<area>]"},
	{"es", "turn_off", DevSpeaker, "apaga [el] altavoz [<area>]"},
	{"es", "turn_on", DevPlug, "(enciende|activa) [el] enchufe [<area>]"},
	{"es", "turn_off", DevPlug, "(apaga|desactiva) [el] enchufe [<area>]"},
	// Multimedia y volumen.
	{"es", "media_pause", "", "(pausa|para|detén|pon en pausa) <media>"},
	{"es", "media_resume", "", "(reanuda|continúa|sigue con) <media>"},
	{"es", "media_resume", "", "quita la pausa"},
	{"es", "media_next", "", "(siguiente|pasa|salta) [(canción|pista|capítulo|episodio)]"},
	{"es", "media_next", "", "(pon|pasa a) la siguiente [(canción|pista)]"},
	{"es", "media_previous", "", "(canción|pista) anterior"},
	{"es", "media_previous", "", "(pon|vuelve a) la [(canción|pista)] anterior"},
	{"es", "volume_up", "", "(sube|aumenta) el volumen"},
	{"es", "volume_up", DevTV, "(sube|aumenta) el volumen de la (tele|televisión)"},
	{"es", "volume_up", DevSpeaker, "(sube|aumenta) el volumen del altavoz"},
	{"es", "volume_up", "", "más (alto|volumen)"},
	{"es", "volume_down", "", "(baja|reduce) el volumen"},
	{"es", "volume_down", DevTV, "(baja|reduce) el volumen de la (tele|televisión)"},
	{"es", "volume_down", DevSpeaker, "(baja|reduce) el volumen del altavoz"},
	{"es", "volume_down", "", "(más bajo|menos volumen)"},
	{"es", "volume_set", "", "(pon|ajusta|sube|baja) el volumen <a> {value} [<porc>]"},
	{"es", "volume_set", DevTV, "(pon|ajusta|sube|baja) el volumen de la (tele|televisión) <a> {value} [<porc>]"},
	{"es", "volume_mute", "", "(silencia|mutea) [(la música|el sonido)]"},
	{"es", "volume_mute", DevTV, "(silencia|mutea) la (tele|televisión)"},
	{"es", "volume_mute", "", "(silencio|quita el sonido)"},
	{"es", "volume_unmute", "", "((activa|devuelve|pon) el sonido|quita el silencio)"},

	// English.
	{"en", "turn_on", DevLight, "(turn|switch) on [the] <light> [<area>]"},
	{"en", "turn_on", DevLight, "(turn|switch) [the] <light> [<area>] on"},
	{"en", "turn_on", DevLight, "(turn|switch) on the {area} <light>"},
	{"en", "turn_on", DevLight, "(turn|switch) [the] {area} <light> on"},
	{"en", "turn_on", DevLight, "<light> on [<area>]"},
	{"en", "turn_off", DevLight, "(turn|switch) off [the] <light> [<area>]"},
	{"en", "turn_off", DevLight, "(turn|switch) [the] <light> [<area>] off"},
	{"en", "turn_off", DevLight, "(turn|switch) off the {area} <light>"},
	{"en", "turn_off", DevLight, "(turn|switch) [the] {area} <light> off"},
	{"en", "turn_off", DevLight, "<light> off [<area>]"},
	{"en", "set_brightness", DevLight, "(set|dim|turn|put) [the] <light> [<area>] (to|at) {value} [<pct>]"},
	{"en", "set_brightness", DevLight, "set [the] brightness [<area>] to {value} [<pct>]"},
	{"en", "brightness_up", DevLight, "(brighten|turn up) [the] <light> [<area>]"},
	{"en", "brightness_up", DevLight, "(increase|raise|turn up) [the] brightness [<area>]"},
	{"en", "brightness_up", DevLight, "more light [<area>]"},
	{"en", "brightness_down", DevLight, "(dim|turn down|lower) [the] <light> [<area>]"},
	{"en", "brightness_down", DevLight, "(decrease|lower|reduce|turn down) [the] brightness [<area>]"},
	{"en", "brightness_down", DevLight, "less light [<area>]"},
	{"en", "set_color", DevLight, "(set|make|change|turn) [the] <light> [<area>] [to] {color}"},
	{"en", "set_color", DevLight, "change the [light] color [<area>] to {color}"},
	{"en", "turn_on", DevThermostat, "(turn|switch) on [the] (heating|heat|thermostat|air conditioning|ac) [<area>]"},
	{"en", "turn_off", DevThermostat, "(turn|switch) off [the] (heating|heat|thermostat|air conditioning|ac) [<area>]"},
	{"en", "set_temperature", DevThermostat, "(set|change|put|turn up|turn down|raise|lower) [the] <clima> [<area>] to {value} [<deg>]"},
	{"en", "set_temperature", DevThermostat, "make it {value} <deg> [<area>]"},
	{"en", "temperature_up", DevThermostat, "(turn up|raise|increase) the <clima> [<area>]"},
	{"en", "temperature_down", DevThermostat, "(turn down|lower|decrease) the <clima> [<area>]"},
	{"en", "get_temperature", DevThermostat, "(what's|what is) the temperature [<area>]"},
	{"en", "get_temperature", DevThermostat, "(how (warm|hot|cold) is it|what temperature is it) [<area>]"},
	{"en", "cover_open", DevBlinds, "(open|raise) <cover> [<area>]"},
	{"en", "cover_close", DevBlinds, "(close|lower|shut) <cover> [<area>]"},
	{"en", "cover_set_position", DevBlinds, "(set|open|close|raise|lower|put) <cover> [<area>] to {value} [<pct>]"},
	{"en", "lock", DevLock, "lock [the] [front] door"},
	{"en", "lock", DevLock, "lock up"},
	{"en", "unlock", DevLock, "(unlock|open) [the] [front] door"},
	{"en", "alarm_arm", DevAlarm, "(arm|activate|turn on) the (alarm|security system)"},
	{"en", "alarm_disarm", DevAlarm, "(disarm|deactivate|turn off|disable) the (alarm|security system)"},
	{"en", "turn_on", DevTV, "(turn|switch) on the (tv|television)"},
	{"en", "turn_on", DevTV, "(turn|switch) the (tv|television) on"},
	{"en", "turn_off", DevTV, "(turn|switch) off the (tv|television)"},
	{"en", "turn_off", DevTV, "(turn|switch) the (tv|television) off"},
	{"en", "turn_on", DevFan, "(turn|switch) on the fan [<area>]"},
	{"en", "turn_off", DevFan, "(turn|switch) off the fan [<area>]"},
	{"en", "fan_set_speed", DevFan, "set the fan [speed] to {value} [<pct>]"},
	{"en", "turn_on", DevSpeaker, "(turn|switch) on the speaker [<area>]"},
	{"en", "turn_off", DevSpeaker, "(turn|switch) off the speaker [<area>]"},
	{"en", "turn_on", DevPlug, "(turn|switch) on the (plug|smart plug|outlet) [<area>]"},
	{"en", "turn_off", DevPlug, "(turn|switch) off the (plug|smart plug|outlet) [<area>]"},
	{"en", "media_pause", "", "pause <media>"},
	{"en", "media_resume", "", "(resume|continue|unpause) <media>"},
	{"en", "media_next", "", "(next|skip) [(song|track|episode)]"},
	{"en", "media_next", "", "play the next (song|track|episode)"},
	{"en", "media_previous", "", "(previous|last) (song|track)"},
	{"en", "media_previous", "", "play the previous (song|track)"},
	{"en", "volume_up", "", "(turn up|raise|increase) the volume"},
	{"en", "volume_up", DevTV, "(turn up|raise|increase) the volume on the (tv|television)"},
	{"en", "volume_up", "", "(louder|volume up)"},
	{"en", "volume_down", "", "(turn down|lower|decrease|reduce) the volume"},
	{"en", "volume_down", DevTV, "(turn down|lower|decrease|reduce) the volume on the (tv|television)"},
	{"en", "volume_down", "", "(quieter|volume down)"},
	{"en", "volume_set", "", "(set|change|turn|put) the volume to {value} [<pct>]"},
	{"en", "volume_set", DevTV, "(set|change|turn|put) the volume on the (tv|television) to {value} [<pct>]"},
	{"en", "volume_mute", "", "mute [the] [(music|sound)]"},
	{"en", "volume_mute", DevTV, "mute the (tv|television)"},
	{"en", "volume_unmute", "", "unmute [the] [(music|sound)]"},
	{"en", "volume_unmute", DevTV, "unmute the (tv|television)"},
}

// DemoRules devuelve las reglas de expansión de las plantillas de la demo.
func DemoRules(lang string) map[string]string { return demoRules[lang] }
