// Package room es la habitación de la demo: un simulador pequeño de sus
// dispositivos (luces, termostato, persianas, tele, altavoz, cerradura, alarma,
// ventilador y enchufe) y un servidor web que la dibuja, recibe órdenes, las
// pasa por la cascada de pkg/domotica y enseña qué capa decidió y cuánto tardó.
// Ver docs/demo-domotica.md.
package room

import (
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/juan52878911/kindling/pkg/domotica"
)

// Zonas de la habitación. Una orden a otra zona («la luz del garaje») no
// encuentra dispositivo y lo dice, en vez de encender otra cosa.
var (
	LightAreas = []string{"living_room", "kitchen", "bedroom"}
	BlindAreas = []string{"living_room", "bedroom"}
)

// Light es una luz regulable con color.
type Light struct {
	Area       string `json:"area"`
	On         bool   `json:"on"`
	Brightness int    `json:"brightness"` // 0-100
	Color      string `json:"color"`
}

// Blind es una persiana: 0 cerrada, 100 abierta.
type Blind struct {
	Area     string `json:"area"`
	Position int    `json:"position"`
}

// Player es la tele o el altavoz.
type Player struct {
	On      bool `json:"on"`
	Playing bool `json:"playing"`
	Volume  int  `json:"volume"`
	Muted   bool `json:"muted"`
	Track   int  `json:"track"`
}

// State es el estado de todos los dispositivos.
type State struct {
	Lights     []Light `json:"lights"`
	Thermostat struct {
		On      bool    `json:"on"`
		Current float64 `json:"current"`
		Target  float64 `json:"target"`
	} `json:"thermostat"`
	Blinds  []Blind `json:"blinds"`
	TV      Player  `json:"tv"`
	Speaker Player  `json:"speaker"`
	Locked  bool    `json:"locked"`
	Armed   bool    `json:"armed"`
	Fan     int     `json:"fan"` // velocidad 0-100; 0 = apagado
	Plug    bool    `json:"plug"`
	Version int     `json:"version"`
}

// Initial es el estado con el que arranca (y vuelve con «reset») la demo.
func Initial() State {
	var s State
	s.Lights = []Light{
		{Area: "living_room", On: true, Brightness: 70, Color: "warm"},
		{Area: "kitchen", Brightness: 100, Color: "white"},
		{Area: "bedroom", Brightness: 60, Color: "warm"},
	}
	s.Thermostat.On, s.Thermostat.Current, s.Thermostat.Target = true, 19.5, 21
	s.Blinds = []Blind{{Area: "living_room", Position: 100}, {Area: "bedroom", Position: 100}}
	s.TV = Player{Volume: 25, Track: 1}
	s.Speaker = Player{On: true, Playing: true, Volume: 30, Track: 1}
	s.Locked = true
	return s
}

// Effect es lo que hizo una acción: qué dispositivos cambiaron (para animarlos)
// y la frase de vuelta en los dos idiomas.
type Effect struct {
	Intent  string   `json:"intent"`
	OK      bool     `json:"ok"`
	Changed []string `json:"changed"`
	SayES   string   `json:"say_es"`
	SayEN   string   `json:"say_en"`
}

// Room es la habitación simulada, segura para usar desde varias goroutines.
type Room struct {
	mu sync.Mutex
	s  State
}

// New crea la habitación en su estado inicial.
func New() *Room { return &Room{s: Initial()} }

// Snapshot devuelve una copia del estado.
func (r *Room) Snapshot() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return clone(r.s)
}

func clone(s State) State {
	s.Lights = append([]Light(nil), s.Lights...)
	s.Blinds = append([]Blind(nil), s.Blinds...)
	return s
}

// Reset vuelve al estado inicial.
func (r *Room) Reset() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.s.Version
	r.s = Initial()
	r.s.Version = v + 1
	return clone(r.s)
}

// Tick acerca la temperatura actual a la objetivo (0,1 °C por llamada) con el
// termostato encendido; sin él, deriva hacia 17 °C. Devuelve si cambió.
func (r *Room) Tick() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := &r.s.Thermostat
	goal := 17.0
	if t.On {
		goal = t.Target
	}
	d := goal - t.Current
	if math.Abs(d) < 0.05 {
		return false
	}
	t.Current = math.Round((t.Current+math.Copysign(math.Min(0.1, math.Abs(d)), d))*10) / 10
	r.s.Version++
	return true
}

// Apply ejecuta las acciones en orden y devuelve lo que hizo cada una.
func (r *Room) Apply(acts []domotica.Action) ([]Effect, State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Effect, 0, len(acts))
	for _, a := range acts {
		e := r.apply(a)
		e.Intent = a.Intent
		out = append(out, e)
	}
	if len(acts) > 0 {
		r.s.Version++
	}
	return out, clone(r.s)
}

func ok(changed []string, es, en string) Effect {
	return Effect{OK: true, Changed: changed, SayES: es, SayEN: en}
}

func fail(es, en string) Effect { return Effect{SayES: es, SayEN: en} }

// Nombres de zona para las frases.
var areaName = map[string][2]string{
	"living_room": {"el salón", "the living room"},
	"kitchen":     {"la cocina", "the kitchen"},
	"bedroom":     {"el dormitorio", "the bedroom"},
}

func areaSay(area string) (string, string) {
	if n, ok := areaName[area]; ok {
		return n[0], n[1]
	}
	if area == "" {
		return "toda la habitación", "the whole room"
	}
	return strings.ReplaceAll(area, "_", " "), strings.ReplaceAll(area, "_", " ")
}

// wholeRoom: sin zona, o una zona que abarca todo, vale para todos los
// dispositivos de ese tipo.
func wholeRoom(area string) bool { return area == "" || area == "house" || area == "room" }

func (r *Room) lights(area string) []*Light {
	var out []*Light
	for i := range r.s.Lights {
		if wholeRoom(area) || r.s.Lights[i].Area == area {
			out = append(out, &r.s.Lights[i])
		}
	}
	return out
}

func (r *Room) blinds(area string) []*Blind {
	var out []*Blind
	for i := range r.s.Blinds {
		if wholeRoom(area) || r.s.Blinds[i].Area == area {
			out = append(out, &r.s.Blinds[i])
		}
	}
	return out
}

func lightIDs(ls []*Light) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = "light." + l.Area
	}
	return out
}

func clamp(v, lo, hi int) int { return max(lo, min(hi, v)) }

func step(s domotica.Slots, def int) int {
	if s.HasValue && s.Value > 0 {
		return int(math.Round(s.Value))
	}
	return def
}

// player elige la tele o el altavoz: el que nombra la orden, si no el que está
// sonando (la tele primero), si no el altavoz.
func (r *Room) player(dev string) (*Player, string, [2]string) {
	tv, sp := &r.s.TV, &r.s.Speaker
	switch {
	case dev == domotica.DevTV, dev == "" && tv.On:
		return tv, "tv", [2]string{"la tele", "the TV"}
	}
	return sp, "speaker", [2]string{"el altavoz", "the speaker"}
}

func (r *Room) apply(a domotica.Action) Effect {
	s := a.Slots
	aes, aen := areaSay(s.Area)
	switch a.Intent {
	case "turn_on", "turn_off":
		on := a.Intent == "turn_on"
		wes, wen := "apagado", "off"
		if on {
			wes, wen = "encendido", "on"
		}
		switch s.Device {
		case domotica.DevLight, "":
			ls := r.lights(s.Area)
			if len(ls) == 0 {
				return fail("No hay luces en "+aes+".", "There are no lights in "+aen+".")
			}
			for _, l := range ls {
				l.On = on
				if on && l.Brightness == 0 {
					l.Brightness = 100
				}
				if on && s.Color != "" {
					l.Color = s.Color
				}
			}
			if on {
				return ok(lightIDs(ls), "Luz encendida en "+aes+".", "Lights on in "+aen+".")
			}
			return ok(lightIDs(ls), "Luz apagada en "+aes+".", "Lights off in "+aen+".")
		case domotica.DevThermostat:
			r.s.Thermostat.On = on
			return ok([]string{"thermostat"}, "Termostato "+wes+".", "Thermostat "+wen+".")
		case domotica.DevTV:
			r.s.TV.On, r.s.TV.Playing = on, on
			if on {
				r.s.Speaker.Playing = false // la tele manda: no suenan las dos a la vez
			}
			return ok([]string{"tv", "speaker"}, "Tele "+strings.TrimSuffix(wes, "o")+"a.", "TV "+wen+".")
		case domotica.DevSpeaker:
			r.s.Speaker.On, r.s.Speaker.Playing = on, on
			return ok([]string{"speaker"}, "Altavoz "+wes+".", "Speaker "+wen+".")
		case domotica.DevFan:
			if on {
				r.s.Fan = max(r.s.Fan, 50)
			} else {
				r.s.Fan = 0
			}
			return ok([]string{"fan"}, "Ventilador "+wes+".", "Fan "+wen+".")
		case domotica.DevPlug:
			r.s.Plug = on
			return ok([]string{"plug"}, "Enchufe "+wes+".", "Plug "+wen+".")
		}
		return fail("Ese dispositivo no se enciende ni se apaga.", "That device can't be switched on or off.")
	case "brightness_up", "brightness_down", "set_brightness":
		ls := r.lights(s.Area)
		if len(ls) == 0 {
			return fail("No hay luces en "+aes+".", "There are no lights in "+aen+".")
		}
		if a.Intent != "set_brightness" && wholeRoom(s.Area) {
			// «Sube la luz» sin zona: las que están encendidas; si ninguna, todas.
			var lit []*Light
			for _, l := range ls {
				if l.On {
					lit = append(lit, l)
				}
			}
			if len(lit) > 0 {
				ls = lit
			}
		}
		var b int
		for _, l := range ls {
			switch a.Intent {
			case "brightness_up":
				if !l.On {
					l.On, l.Brightness = true, 0
				}
				l.Brightness = clamp(l.Brightness+step(s, 20), 5, 100)
			case "brightness_down":
				l.Brightness = clamp(l.Brightness-step(s, 20), 5, 100)
			default:
				l.Brightness = clamp(int(math.Round(s.Value)), 0, 100)
				l.On = l.Brightness > 0
			}
			b = l.Brightness
		}
		return ok(lightIDs(ls), fmt.Sprintf("Brillo al %d %% en %s.", b, aes), fmt.Sprintf("Brightness at %d%% in %s.", b, aen))
	case "set_color":
		ls := r.lights(s.Area)
		if len(ls) == 0 {
			return fail("No hay luces en "+aes+".", "There are no lights in "+aen+".")
		}
		for _, l := range ls {
			l.On, l.Color = true, s.Color
			if l.Brightness == 0 {
				l.Brightness = 100
			}
		}
		return ok(lightIDs(ls), "Luz "+colorES[s.Color]+" en "+aes+".", "Lights "+s.Color+" in "+aen+".")
	case "set_temperature", "temperature_up", "temperature_down":
		t := &r.s.Thermostat
		switch a.Intent {
		case "set_temperature":
			t.Target = s.Value
		case "temperature_up":
			t.Target += float64(step(s, 1))
		default:
			t.Target -= float64(step(s, 1))
		}
		t.Target = math.Max(10, math.Min(30, t.Target))
		t.On = true
		return ok([]string{"thermostat"}, fmt.Sprintf("Termostato a %.1f °C.", t.Target), fmt.Sprintf("Thermostat set to %.1f °C.", t.Target))
	case "get_temperature":
		t := r.s.Thermostat
		return ok([]string{"thermostat"}, fmt.Sprintf("Hace %.1f °C; el objetivo es %.1f °C.", t.Current, t.Target),
			fmt.Sprintf("It's %.1f °C; the target is %.1f °C.", t.Current, t.Target))
	case "cover_open", "cover_close", "cover_set_position":
		bs := r.blinds(s.Area)
		if len(bs) == 0 {
			return fail("No hay persianas en "+aes+".", "There are no blinds in "+aen+".")
		}
		p := 100
		switch a.Intent {
		case "cover_close":
			p = 0
		case "cover_set_position":
			p = clamp(int(math.Round(s.Value)), 0, 100)
		}
		ids := make([]string, len(bs))
		for i, b := range bs {
			b.Position = p
			ids[i] = "blind." + b.Area
		}
		return ok(ids, fmt.Sprintf("Persianas al %d %% en %s.", p, aes), fmt.Sprintf("Blinds at %d%% in %s.", p, aen))
	case "lock", "unlock":
		r.s.Locked = a.Intent == "lock"
		if r.s.Locked {
			return ok([]string{"lock"}, "Puerta cerrada con llave.", "Door locked.")
		}
		return ok([]string{"lock"}, "Puerta abierta.", "Door unlocked.")
	case "alarm_arm", "alarm_disarm":
		r.s.Armed = a.Intent == "alarm_arm"
		if r.s.Armed {
			return ok([]string{"alarm"}, "Alarma activada.", "Alarm armed.")
		}
		return ok([]string{"alarm"}, "Alarma desactivada.", "Alarm disarmed.")
	case "media_pause", "media_resume", "media_next", "media_previous":
		p, id, n := r.player(s.Device)
		switch a.Intent {
		case "media_pause":
			p.Playing = false
			return ok([]string{id}, "Pausa en "+n[0]+".", "Paused "+n[1]+".")
		case "media_resume":
			p.On, p.Playing = true, true
			return ok([]string{id}, "Reanudo "+n[0]+".", "Resumed "+n[1]+".")
		case "media_next":
			p.Track++
		default:
			p.Track = max(1, p.Track-1)
		}
		p.On, p.Playing = true, true
		return ok([]string{id}, fmt.Sprintf("Pista %d en %s.", p.Track, n[0]), fmt.Sprintf("Track %d on %s.", p.Track, n[1]))
	case "volume_up", "volume_down", "volume_set", "volume_mute", "volume_unmute":
		p, id, n := r.player(s.Device)
		switch a.Intent {
		case "volume_up":
			p.Volume, p.Muted = clamp(p.Volume+step(s, 10), 0, 100), false
		case "volume_down":
			p.Volume = clamp(p.Volume-step(s, 10), 0, 100)
		case "volume_set":
			p.Volume, p.Muted = clamp(int(math.Round(s.Value)), 0, 100), false
		case "volume_mute":
			p.Muted = true
			return ok([]string{id}, "Silencio en "+n[0]+".", "Muted "+n[1]+".")
		default:
			p.Muted = false
		}
		return ok([]string{id}, fmt.Sprintf("Volumen de %s al %d %%.", n[0], p.Volume), fmt.Sprintf("%s volume at %d%%.", upper(n[1]), p.Volume))
	case "fan_set_speed":
		r.s.Fan = clamp(int(math.Round(s.Value)), 0, 100)
		return ok([]string{"fan"}, fmt.Sprintf("Ventilador al %d %%.", r.s.Fan), fmt.Sprintf("Fan at %d%%.", r.s.Fan))
	}
	return fail("No sé hacer eso.", "I don't know how to do that.")
}

var colorES = map[string]string{
	"white": "blanca", "black": "negra", "red": "roja", "orange": "naranja", "yellow": "amarilla", "green": "verde",
	"blue": "azul", "purple": "morada", "brown": "marrón", "pink": "rosa", "cyan": "cian", "warm": "cálida", "cool": "fría",
}

func upper(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
