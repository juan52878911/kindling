package domotica

import (
	"github.com/juan52878911/kindling/pkg/chispa/slots"
)

// Línea base de reglas por palabras clave: lo que se escribiría a mano en una
// tarde (dispositivo + verbo) con los huecos sacados del léxico. No es una
// capa de la cascada; está para saber cuánto aporta cada capa de verdad.

type kwTokens map[string]bool

func (k kwTokens) any(ws ...string) bool {
	for _, w := range ws {
		if k[w] {
			return true
		}
	}
	return false
}

// Keywords decide con reglas. Siempre contesta (no sabe dudar).
func Keywords(text string) (string, Slots) {
	toks := slots.Tokenize(text)
	k := kwTokens{}
	for _, t := range toks {
		k[t.Norm] = true
	}
	var s Slots
	for _, sp := range FindSpans(text) {
		switch sp.Slot {
		case SlotArea:
			if s.Area == "" {
				s.Area, _ = CanonArea(sp.Text)
			}
		case SlotDevice:
			if s.Device == "" {
				s.Device, _ = CanonDevice(sp.Text)
			}
		case SlotColor:
			if s.Color == "" {
				s.Color, _ = CanonColor(sp.Text)
			}
		}
	}
	var tk slots.Tokenizer
	tk.Run(text)
	for i := 0; i < tk.Len(); i++ {
		if v, n, ok := ParseNumberAt(&tk, i); ok {
			s.Value, s.HasValue, s.Unit = v, true, UnitAfter(&tk, i+n-1)
			break
		}
	}
	up := k.any("sube", "aumenta", "subir", "up", "raise", "increase", "louder", "brighten", "mas", "more")
	down := k.any("baja", "reduce", "bajar", "down", "lower", "decrease", "quieter", "dim", "menos", "less", "atenua")
	on := k.any("enciende", "prende", "activa", "conecta", "encender", "on", "start")
	off := k.any("apaga", "desactiva", "desconecta", "apagar", "off")
	open := k.any("abre", "abrir", "open", "unlock", "desbloquea")
	shut := k.any("cierra", "cerrar", "close", "shut", "lock", "bloquea")
	intent := OutOfScope
	switch {
	case k.any("volumen", "volume", "louder", "quieter"):
		switch {
		case s.HasValue:
			intent = "volume_set"
		case up:
			intent = "volume_up"
		case down:
			intent = "volume_down"
		}
	case k.any("silencia", "silencio", "mute", "mutea"):
		intent = "volume_mute"
	case k.any("unmute"):
		intent = "volume_unmute"
	case k.any("pausa", "pause"):
		intent = "media_pause"
	case k.any("reanuda", "resume", "continua", "unpause"):
		intent = "media_resume"
	case k.any("siguiente", "next", "skip"):
		intent = "media_next"
	case k.any("anterior", "previous"):
		intent = "media_previous"
	case s.Device == DevAlarm:
		switch {
		case off || k.any("desarma", "disarm", "quita"):
			intent = "alarm_disarm"
		case on || k.any("arma", "arm", "pon"):
			intent = "alarm_arm"
		}
	case s.Device == DevBlinds:
		switch {
		case s.HasValue:
			intent = "cover_set_position"
		case open || up:
			intent = "cover_open"
		case shut || down:
			intent = "cover_close"
		}
	case s.Device == DevLock:
		switch {
		case open:
			intent = "unlock"
		case shut:
			intent = "lock"
		}
	case s.Device == DevThermostat || k.any("temperatura", "temperature", "grados", "degrees"):
		switch {
		case s.HasValue:
			intent = "set_temperature"
		case on:
			intent, s.Device = "turn_on", DevThermostat
		case off:
			intent, s.Device = "turn_off", DevThermostat
		case up:
			intent = "temperature_up"
		case down:
			intent = "temperature_down"
		case k.any("que", "cual", "what", "how", "cuantos"):
			intent = "get_temperature"
		}
	case s.Device == DevFan && s.HasValue:
		intent = "fan_set_speed"
	case s.Color != "":
		intent = "set_color"
	case s.Device == DevLight || k.any("brillo", "brightness"):
		switch {
		case on:
			intent = "turn_on"
		case off:
			intent = "turn_off"
		case s.HasValue:
			intent = "set_brightness"
		case up:
			intent = "brightness_up"
		case down:
			intent = "brightness_down"
		}
	case s.Device != "":
		switch {
		case on:
			intent = "turn_on"
		case off:
			intent = "turn_off"
		}
	}
	if intent == OutOfScope {
		return intent, Slots{}
	}
	return intent, Resolve(intent, s)
}
