// Package domotica decide qué hacer con una orden de voz (ya en texto) en la
// habitación de demo: luces, termostato, persianas, tele, cerradura, alarma,
// ventilador, altavoz y enchufe. La decisión pasa por capas de coste
// creciente y cada una solo se activa si su evaluación mejora la anterior:
//
//  1. emparejador de plantillas (microsegundos, exacto para los textos de la
//     demo; Matcher),
//  2. JEV: intención con pkg/jev y huecos con pkg/jev/slots (microsegundos),
//  3. y 4. —un codificador de frases y un LLM pequeño con salida JSON— son
//     fases posteriores: aquí solo se marca qué debe escalar a ellas.
//
// Ver docs/domotica.md y docs/DOMOTICA-EVAL.md.
package domotica

import "sort"

// OutOfScope es la intención de todo lo que la habitación no sabe hacer.
const OutOfScope = "out_of_scope"

// Slot names del esquema unificado.
const (
	SlotDevice = "device"
	SlotArea   = "area"
	SlotValue  = "value"
	SlotColor  = "color"
)

// IntentInfo describe una intención de la taxonomía.
type IntentInfo struct {
	Name string
	Desc string
	// Device es el dispositivo implícito cuando la orden no lo nombra
	// («sube el brillo» es la luz). Vacío: no hay uno implícito.
	Device string
	// Unit es la unidad por defecto del valor («a 22» en el termostato son °C).
	Unit string
	// NeedsValue / NeedsColor: sin ellos la orden no se puede ejecutar y la
	// decisión no es confiada aunque la intención lo sea.
	NeedsValue bool
	NeedsColor bool
}

// Intents es la taxonomía de la habitación de demo. El mapeo desde MASSIVE y
// Home Assistant está en docs/domotica-datos.md.
var Intents = []IntentInfo{
	{Name: "turn_on", Desc: "switch a device on (default: the light)", Device: DevLight},
	{Name: "turn_off", Desc: "switch a device off (default: the light)", Device: DevLight},
	{Name: "brightness_up", Desc: "brighter light, optional step", Device: DevLight, Unit: UnitPercent},
	{Name: "brightness_down", Desc: "dimmer light, optional step", Device: DevLight, Unit: UnitPercent},
	{Name: "set_brightness", Desc: "light brightness to a value", Device: DevLight, Unit: UnitPercent, NeedsValue: true},
	{Name: "set_color", Desc: "light color", Device: DevLight, NeedsColor: true},
	{Name: "set_temperature", Desc: "thermostat to a temperature", Device: DevThermostat, Unit: UnitCelsius, NeedsValue: true},
	{Name: "temperature_up", Desc: "warmer, no target value", Device: DevThermostat, Unit: UnitCelsius},
	{Name: "temperature_down", Desc: "cooler, no target value", Device: DevThermostat, Unit: UnitCelsius},
	{Name: "get_temperature", Desc: "ask the current temperature", Device: DevThermostat},
	{Name: "cover_open", Desc: "open the blinds", Device: DevBlinds},
	{Name: "cover_close", Desc: "close the blinds", Device: DevBlinds},
	{Name: "cover_set_position", Desc: "blinds to a position", Device: DevBlinds, Unit: UnitPercent, NeedsValue: true},
	{Name: "lock", Desc: "lock the door", Device: DevLock},
	{Name: "unlock", Desc: "unlock the door", Device: DevLock},
	{Name: "alarm_arm", Desc: "arm the security alarm", Device: DevAlarm},
	{Name: "alarm_disarm", Desc: "disarm the security alarm", Device: DevAlarm},
	{Name: "media_pause", Desc: "pause playback"},
	{Name: "media_resume", Desc: "resume playback"},
	{Name: "media_next", Desc: "next track or episode"},
	{Name: "media_previous", Desc: "previous track"},
	{Name: "volume_up", Desc: "louder, optional step", Unit: UnitPercent},
	{Name: "volume_down", Desc: "quieter, optional step", Unit: UnitPercent},
	{Name: "volume_set", Desc: "volume to a value", Unit: UnitPercent, NeedsValue: true},
	{Name: "volume_mute", Desc: "mute"},
	{Name: "volume_unmute", Desc: "unmute"},
	{Name: "fan_set_speed", Desc: "fan speed to a value", Device: DevFan, Unit: UnitPercent, NeedsValue: true},
	{Name: OutOfScope, Desc: "anything the room cannot do (timers, music search, weather, clock alarms…)"},
}

var intentByName = func() map[string]*IntentInfo {
	m := map[string]*IntentInfo{}
	for i := range Intents {
		m[Intents[i].Name] = &Intents[i]
	}
	return m
}()

// Intent devuelve la descripción de una intención (nil si no existe).
func Intent(name string) *IntentInfo { return intentByName[name] }

// IntentNames: todas las intenciones, ordenadas.
func IntentNames() []string {
	out := make([]string, 0, len(Intents))
	for _, it := range Intents {
		out = append(out, it.Name)
	}
	sort.Strings(out)
	return out
}

// Resolve completa los huecos implícitos de una intención: el dispositivo por
// defecto y la unidad del valor. Se aplica igual al oro y a la predicción
// antes de comparar, así que «apaga la cocina» (sin dispositivo) y el oro
// {turn_off, light, kitchen} coinciden. Fuera de ámbito no lleva huecos.
func Resolve(intent string, s Slots) Slots {
	it := intentByName[intent]
	if it == nil || intent == OutOfScope {
		return Slots{}
	}
	if s.Device == "" {
		s.Device = it.Device
	}
	if s.HasValue && s.Unit == "" {
		s.Unit = it.Unit
	}
	if it.Unit == "" {
		// Solo las intenciones con unidad aceptan un valor: el «uno» de
		// «enciende el enchufe uno» es un nombre, no una cantidad.
		s.Value, s.HasValue = 0, false
	}
	if !s.HasValue {
		s.Unit = ""
	}
	if !it.NeedsColor && intent != "turn_on" {
		// Un color solo significa algo al fijar el color (o al encender:
		// «enciende la luz en azul»); en otra orden es ruido del etiquetador.
		s.Color = ""
	}
	return s
}

// Complete dice si la orden tiene lo que necesita para ejecutarse.
func Complete(intent string, s Slots) bool {
	it := intentByName[intent]
	if it == nil {
		return false
	}
	if it.NeedsValue && !s.HasValue {
		return false
	}
	if it.NeedsColor && s.Color == "" {
		return false
	}
	return true
}
