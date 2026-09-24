package domotica

import (
	"fmt"
	"strings"
	"sync"

	"github.com/juan52878911/kindling/pkg/jev/slots"
)

// Marcadores con los que el emparejador sustituye zonas, números y colores.
// No son palabras de ningún idioma, así que no chocan con el texto literal.
const (
	phArea  = "zzarea"
	phNum   = "zznum"
	phColor = "zzcolor"
)

// Cortesías y palabras de activación que no cambian la orden. Se quitan antes
// de emparejar, en cualquier posición.
var skipPhrases = [][]string{
	{"por", "favor"}, {"porfa"}, {"porfavor"}, {"gracias"}, {"please"}, {"pls"}, {"thanks"}, {"thank", "you"},
	{"puedes"}, {"podrias"}, {"me", "puedes"}, {"me", "podrias"}, {"can", "you"}, {"could", "you"}, {"would", "you"},
	{"olly"}, {"alexa"}, {"siri"}, {"oye"}, {"hey"}, {"ok"}, {"okay"},
}

type matchEntry struct {
	intent, device                  string
	hasArea, hasValue, hasColor, ok bool
}

// Match es el resultado del emparejador.
type Match struct {
	Intent string
	Slots  Slots
	OK     bool // false: ninguna orden de la demo encaja
}

// Matcher es la capa 1: órdenes de la demo por coincidencia exacta tras
// normalizar. Es inmutable tras NewMatcher y seguro para uso concurrente.
type Matcher struct {
	byKey map[string]matchEntry
	pool  sync.Pool
}

type matchScratch struct {
	tk  slots.Tokenizer
	key []byte
	tmp []byte
}

// NewMatcher compila las plantillas de la demo. Falla si dos plantillas dan
// la misma frase con resultados distintos: la demo tiene que ser inequívoca.
func NewMatcher(tpls []DemoTemplate) (*Matcher, error) {
	m := &Matcher{byKey: map[string]matchEntry{}}
	grammars := map[string]*Grammar{}
	var s matchScratch
	for _, t := range tpls {
		g := grammars[t.Lang]
		if g == nil {
			g = &Grammar{Lang: t.Lang, Rules: DemoRules(t.Lang)}
			grammars[t.Lang] = g
		}
		if Intent(t.Intent) == nil {
			return nil, fmt.Errorf("demo template %q: unknown intent %q", t.Template, t.Intent)
		}
		n, err := Parse(t.Template)
		if err != nil {
			return nil, err
		}
		sents, err := g.Enumerate(n, 20000, func(list, _ string) string {
			switch list {
			case "area":
				return phArea
			case "value":
				return phNum
			case "color":
				return phColor
			}
			return "zz" + list
		})
		if err != nil {
			return nil, fmt.Errorf("demo template %q: %w", t.Template, err)
		}
		for _, sent := range sents {
			key, _ := m.key(&s, sent)
			k := string(key)
			e := matchEntry{intent: t.Intent, device: t.Device, ok: true,
				hasArea: strings.Contains(k, phArea), hasValue: strings.Contains(k, phNum), hasColor: strings.Contains(k, phColor)}
			if old, dup := m.byKey[k]; dup && (old.intent != e.intent || old.device != e.device) {
				return nil, fmt.Errorf("ambiguous demo phrase %q: %s/%s and %s/%s", sent, old.intent, old.device, e.intent, e.device)
			}
			m.byKey[k] = e
		}
	}
	return m, nil
}

// Size es el número de frases normalizadas distintas que reconoce.
func (m *Matcher) Size() int { return len(m.byKey) }

// captured son los valores que key() encontró en el texto.
type captured struct {
	area, color string
	device      string
	value       float64
	hasValue    bool
	unit        string
	nArea       int
	nNum        int
	nColor      int
}

// key construye la clave normalizada de text en s.key: tokens plegados, sin
// cortesías, con zonas, números y colores sustituidos por su marcador.
func (m *Matcher) key(s *matchScratch, text string) ([]byte, captured) {
	var c captured
	s.tk.Run(text)
	tk := &s.tk
	n := tk.Len()
	s.key = s.key[:0]
	put := func(b []byte) {
		if len(s.key) > 0 {
			s.key = append(s.key, ' ')
		}
		s.key = append(s.key, b...)
	}
	putS := func(x string) {
		if len(s.key) > 0 {
			s.key = append(s.key, ' ')
		}
		s.key = append(s.key, x...)
	}
next:
	for i := 0; i < n; {
		for _, ph := range skipPhrases {
			if matchToks(tk, i, ph) {
				i += len(ph)
				continue next
			}
		}
		// Placeholders ya escritos (al compilar las plantillas).
		if w := tk.Tok(i); len(w) > 2 && w[0] == 'z' && w[1] == 'z' {
			put(w)
			i++
			continue
		}
		if canon, k := m.lookupNgram(s, tk, i, areaIx); k > 0 {
			c.area, c.nArea = canon, c.nArea+1
			putS(phArea)
			i += k
			continue
		}
		if canon, k := m.lookupNgram(s, tk, i, colorIx); k > 0 {
			c.color, c.nColor = canon, c.nColor+1
			putS(phColor)
			i += k
			continue
		}
		if canon, k := m.lookupNgram(s, tk, i, deviceIx); k > 0 {
			// El dispositivo no se sustituye (es parte literal de la orden),
			// pero se recuerda: «reanuda la tele» sale de una plantilla sin
			// dispositivo fijo y aun así habla de la tele.
			if c.device == "" {
				c.device = canon
			}
			for j := i; j < i+k; j++ {
				put(tk.Tok(j))
			}
			i += k
			continue
		}
		if v, k, ok := ParseNumberAt(tk, i); ok {
			c.value, c.hasValue, c.nNum = v, true, c.nNum+1
			c.unit = UnitAfter(tk, i+k-1)
			putS(phNum)
			i += k
			continue
		}
		put(tk.Tok(i))
		i++
	}
	return s.key, c
}

func matchToks(tk *slots.Tokenizer, i int, ph []string) bool {
	if i+len(ph) > tk.Len() {
		return false
	}
	for k, w := range ph {
		if string(tk.Tok(i+k)) != w {
			return false
		}
	}
	return true
}

// lookupNgram busca en ix la forma más larga que empiece en el token i.
func (m *Matcher) lookupNgram(s *matchScratch, tk *slots.Tokenizer, i int, ix index) (string, int) {
	for k := min(ix.maxLen, tk.Len()-i); k >= 1; k-- {
		s.tmp = s.tmp[:0]
		for j := i; j < i+k; j++ {
			if j > i {
				s.tmp = append(s.tmp, ' ')
			}
			s.tmp = append(s.tmp, tk.Tok(j)...)
		}
		if c, ok := ix.full[string(s.tmp)]; ok { // sin reserva
			return c, k
		}
	}
	return "", 0
}

// Match busca text entre las órdenes de la demo. Con el pool caliente no
// reserva memoria. OK=false si no encaja: entonces decide la capa siguiente.
func (m *Matcher) Match(text string) Match {
	s, _ := m.pool.Get().(*matchScratch)
	if s == nil {
		s = &matchScratch{}
	}
	key, c := m.key(s, text)
	e, ok := m.byKey[string(key)]
	m.pool.Put(s)
	if !ok || c.nArea > 1 || c.nNum > 1 || c.nColor > 1 {
		return Match{}
	}
	out := Match{Intent: e.intent, OK: true, Slots: Slots{Device: e.device}}
	if out.Slots.Device == "" {
		out.Slots.Device = c.device
	}
	if e.hasArea {
		out.Slots.Area = c.area
	}
	if e.hasValue {
		out.Slots.Value, out.Slots.HasValue, out.Slots.Unit = c.value, true, c.unit
	}
	if e.hasColor {
		out.Slots.Color = c.color
	}
	out.Slots = Resolve(out.Intent, out.Slots)
	return out
}
