package intent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/juan52878911/kindling/pkg/chispa/slots"
)

// Schema es un Domain de datos: la taxonomía, las plantillas y cómo se
// normalizan los huecos salen de un fichero JSON que escribe quien usa el
// gateway, sin compilar nada:
//
//	{
//	  "out_of_scope": "other",
//	  "lang": "en",
//	  "intents": [
//	    {"name": "open_ticket", "slots": ["queue", "priority"], "required": ["queue"],
//	     "defaults": {"priority": "normal"}}
//	  ],
//	  "values": {"queue": {"billing": ["billing", "invoices"], "support": ["support", "help desk"]}},
//	  "templates": [
//	    {"text": "open a billing ticket", "intent": "open_ticket", "slots": {"queue": "billing"}}
//	  ],
//	  "multi_command": {"verbs": ["open", "close"], "joiners": ["and", "then"]}
//	}
//
// Los huecos son un objeto {nombre: valor} de cadenas. Una plantilla encaja
// si el texto, normalizado como lo tokeniza pkg/chispa/slots (minúsculas,
// sin tildes ni puntuación), es exactamente su texto normalizado. Un hueco
// marcado vale su forma canónica de "values" (si el hueco tiene tabla y la
// forma no está, se ignora) o, sin tabla, su texto normalizado. Una intención
// con "slots" solo acepta esos huecos; "defaults" completa los que faltan y
// "required" dice cuáles necesita para ejecutarse. Para lógica que no cabe en
// datos, el programa que embebe pkg/aigw puede pasar un Domain en Go.
type Schema struct {
	OutOfScopeLabel string                         `json:"out_of_scope"`
	Lang            string                         `json:"lang,omitempty"`
	Intents         []SchemaIntent                 `json:"intents"`
	Values          map[string]map[string][]string `json:"values,omitempty"`
	Templates       []SchemaTemplate               `json:"templates,omitempty"`
	MultiCommand    *SchemaMulti                   `json:"multi_command,omitempty"`

	intents   map[string]*SchemaIntent
	accept    map[string]map[string]bool // intención -> huecos que acepta (nil = todos)
	values    map[string]map[string]string
	templates map[string]SchemaTemplate
	verbs     map[string]bool
	joiners   map[string]bool
}

// SchemaIntent es una intención de un Schema.
type SchemaIntent struct {
	Name     string            `json:"name"`
	Slots    []string          `json:"slots,omitempty"`
	Required []string          `json:"required,omitempty"`
	Defaults map[string]string `json:"defaults,omitempty"`
}

// SchemaTemplate es una orden conocida (capa 1).
type SchemaTemplate struct {
	Text   string            `json:"text"`
	Intent string            `json:"intent"`
	Slots  map[string]string `json:"slots,omitempty"`
}

// SchemaMulti detecta dos órdenes en una frase: un verbo, una conjunción y
// otro verbo después.
type SchemaMulti struct {
	Verbs   []string `json:"verbs"`
	Joiners []string `json:"joiners"`
}

// maxSchemaBytes acota el fichero.
const maxSchemaBytes = 4 << 20

// LoadSchema lee y compila un Schema.
func LoadSchema(path string) (*Schema, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s, err := ParseSchema(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// ParseSchema lee y compila un Schema de r.
func ParseSchema(r io.Reader) (*Schema, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxSchemaBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSchemaBytes {
		return nil, fmt.Errorf("schema larger than %d bytes", maxSchemaBytes)
	}
	var s Schema
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, err
	}
	if err := s.compile(); err != nil {
		return nil, err
	}
	return &s, nil
}

func normText(s string) string {
	toks := slots.Tokenize(s)
	parts := make([]string, len(toks))
	for i, t := range toks {
		parts[i] = t.Norm
	}
	return strings.Join(parts, " ")
}

func (s *Schema) compile() error {
	if s.OutOfScopeLabel == "" {
		return errors.New("out_of_scope is required")
	}
	if len(s.Intents) == 0 {
		return errors.New("no intents")
	}
	s.intents = map[string]*SchemaIntent{}
	s.accept = map[string]map[string]bool{}
	for i := range s.Intents {
		it := &s.Intents[i]
		if it.Name == "" || it.Name == s.OutOfScopeLabel {
			return fmt.Errorf("intent %d: empty name or the out_of_scope label", i+1)
		}
		if s.intents[it.Name] != nil {
			return fmt.Errorf("intent %q twice", it.Name)
		}
		s.intents[it.Name] = it
		if len(it.Slots) > 0 {
			acc := map[string]bool{}
			for _, n := range it.Slots {
				acc[n] = true
			}
			for _, n := range it.Required {
				if !acc[n] {
					return fmt.Errorf("intent %q: required slot %q is not in its slots", it.Name, n)
				}
			}
			for n := range it.Defaults {
				if !acc[n] {
					return fmt.Errorf("intent %q: default for %q, which is not in its slots", it.Name, n)
				}
			}
			s.accept[it.Name] = acc
		}
	}
	s.values = map[string]map[string]string{}
	for slot, canon := range s.Values {
		m := map[string]string{}
		for c, forms := range canon {
			for _, f := range append([]string{c}, forms...) {
				k := normText(f)
				if old, dup := m[k]; dup && old != c {
					return fmt.Errorf("values of %q: %q means both %q and %q", slot, f, old, c)
				}
				m[k] = c
			}
		}
		s.values[slot] = m
	}
	s.templates = map[string]SchemaTemplate{}
	for i, t := range s.Templates {
		if s.intents[t.Intent] == nil {
			return fmt.Errorf("template %d: unknown intent %q", i+1, t.Intent)
		}
		k := normText(t.Text)
		if k == "" {
			return fmt.Errorf("template %d: empty text", i+1)
		}
		if old, dup := s.templates[k]; dup && (old.Intent != t.Intent || !sameMap(s.resolve(old.Intent, old.Slots), s.resolve(t.Intent, t.Slots))) {
			return fmt.Errorf("ambiguous template %q", t.Text)
		}
		s.templates[k] = t
	}
	if m := s.MultiCommand; m != nil {
		s.verbs, s.joiners = map[string]bool{}, map[string]bool{}
		for _, v := range m.Verbs {
			s.verbs[normText(v)] = true
		}
		for _, j := range m.Joiners {
			s.joiners[normText(j)] = true
		}
	}
	return nil
}

// OutOfScope implementa Domain.
func (s *Schema) OutOfScope() string { return s.OutOfScopeLabel }

// DetectLang implementa Domain: el idioma fijo del esquema ("" = ninguno).
func (s *Schema) DetectLang(string) string { return s.Lang }

// Match implementa Domain.
func (s *Schema) Match(text string) (string, any, bool) {
	t, ok := s.templates[normText(text)]
	if !ok {
		return "", nil, false
	}
	return t.Intent, s.resolve(t.Intent, t.Slots), true
}

// Slots implementa Domain.
func (s *Schema) Slots(intent, text string, spans []slots.Span) any {
	got := map[string]string{}
	for _, sp := range spans {
		if sp.Start < 0 || sp.End > len(text) || sp.Start >= sp.End {
			continue
		}
		if _, seen := got[sp.Slot]; seen {
			continue // gana el primero
		}
		v := normText(text[sp.Start:sp.End])
		if tab, ok := s.values[sp.Slot]; ok {
			c, known := tab[v]
			if !known {
				continue
			}
			v = c
		}
		if v != "" {
			got[sp.Slot] = v
		}
	}
	return s.resolve(intent, got)
}

// resolve deja los huecos que acepta la intención y completa los que faltan.
// Siempre un mapa (vacío fuera de ámbito o con una intención desconocida).
func (s *Schema) resolve(intent string, in map[string]string) map[string]string {
	out := map[string]string{}
	it := s.intents[intent]
	if it == nil {
		return out
	}
	acc := s.accept[intent]
	for k, v := range in {
		if acc == nil || acc[k] {
			out[k] = v
		}
	}
	for k, v := range it.Defaults {
		if out[k] == "" {
			out[k] = v
		}
	}
	return out
}

// Check implementa Domain.
func (s *Schema) Check(text, intent string, sl any) string {
	if s.multi(text) {
		return ReasonMultiCommand
	}
	it := s.intents[intent]
	if it == nil {
		return ReasonMissingSlot
	}
	m, _ := sl.(map[string]string)
	for _, n := range it.Required {
		if m[n] == "" {
			return ReasonMissingSlot
		}
	}
	return ""
}

func (s *Schema) multi(text string) bool {
	if len(s.verbs) == 0 {
		return false
	}
	toks := slots.Tokenize(text)
	verbBefore := false
	for i, t := range toks {
		if s.verbs[t.Norm] {
			verbBefore = true
			continue
		}
		if verbBefore && s.joiners[t.Norm] {
			for _, u := range toks[i+1:] {
				if s.verbs[u.Norm] {
					return true
				}
			}
		}
	}
	return false
}

// ParseSlots implementa Domain: un objeto de cadenas (los números se
// aceptan y se escriben como cadena).
func (s *Schema) ParseSlots(raw json.RawMessage) (any, error) {
	out := map[string]string{}
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return out, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("slots: %w", err)
	}
	for k, v := range m {
		switch x := v.(type) {
		case string:
			out[k] = x
		case float64:
			out[k] = strconv.FormatFloat(x, 'g', -1, 64)
		case nil:
		default:
			return nil, fmt.Errorf("slot %q: not a string or number", k)
		}
	}
	return out, nil
}

// Exact implementa Domain: misma intención y mismos huecos una vez
// completados.
func (s *Schema) Exact(goldIntent string, gold any, intent string, got any) bool {
	if goldIntent != intent {
		return false
	}
	g, _ := gold.(map[string]string)
	p, _ := got.(map[string]string)
	return sameMap(s.resolve(goldIntent, g), s.resolve(intent, p))
}

// IntentNames son las intenciones del esquema, más la de fuera de ámbito,
// ordenadas.
func (s *Schema) IntentNames() []string {
	out := []string{s.OutOfScopeLabel}
	for _, it := range s.Intents {
		out = append(out, it.Name)
	}
	sort.Strings(out)
	return out
}

func sameMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}
