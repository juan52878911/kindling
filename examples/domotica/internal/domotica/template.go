package domotica

import (
	"errors"
	"fmt"
	"strings"
)

// Plantillas de frases con el subconjunto de la sintaxis de hassil (la de
// home-assistant/intents) que hace falta aquí:
//
//	(a|b)      alternativas          [a]       opcional
//	(a;b)      permutación («a b» y «b a»)
//	<regla>    regla de expansión    {lista}   valor de una lista
//	{lista:hueco}  valor de una lista que rellena otro hueco
//
// La misma sintaxis sirve para expandir las plantillas de Home Assistant en
// frases de entrenamiento (cmd/domotica-data) y para las órdenes de la demo
// del emparejador (demo.go): una sola implementación, probada una vez.

type nodeKind int

const (
	nText nodeKind = iota
	nSeq
	nAlt
	nPerm
	nRule
	nList
)

// Node es una plantilla analizada.
type Node struct {
	kind  nodeKind
	text  string
	kids  []*Node
	name  string // regla o lista
	slot  string // hueco que rellena la lista
	count float64
}

// maxTemplateBytes acota lo que se analiza; las plantillas reales son de
// decenas de bytes.
const maxTemplateBytes = 8 << 10

// Parse analiza una plantilla.
func Parse(s string) (*Node, error) {
	if len(s) > maxTemplateBytes {
		return nil, errors.New("template too long")
	}
	p := &parser{s: s}
	n, err := p.alts(0, 0)
	if err != nil {
		return nil, err
	}
	if p.i != len(p.s) {
		return nil, fmt.Errorf("unexpected %q at %d in %q", p.s[p.i], p.i, s)
	}
	return n, nil
}

type parser struct {
	s string
	i int
}

// alts lee secuencias separadas por '|' (y por ';' dentro de un grupo, que
// hace una permutación) hasta el cierre stop (0 = fin de la cadena).
func (p *parser) alts(stop byte, depth int) (*Node, error) {
	if depth > 32 {
		return nil, errors.New("template nested too deep")
	}
	var parts []*Node // separadas por ';'
	var opts []*Node  // separadas por '|' dentro de la parte actual
	for {
		seq, err := p.seq(depth)
		if err != nil {
			return nil, err
		}
		opts = append(opts, seq)
		if p.i >= len(p.s) {
			if stop != 0 {
				return nil, fmt.Errorf("missing %q in %q", stop, p.s)
			}
			break
		}
		c := p.s[p.i]
		if c == '|' {
			p.i++
			continue
		}
		if c == ';' && stop == ')' {
			p.i++
			parts = append(parts, altOf(opts))
			opts = nil
			continue
		}
		if c == stop {
			break
		}
		return nil, fmt.Errorf("unexpected %q at %d in %q", c, p.i, p.s)
	}
	last := altOf(opts)
	if len(parts) == 0 {
		return last, nil
	}
	return &Node{kind: nPerm, kids: append(parts, last)}, nil
}

func altOf(opts []*Node) *Node {
	if len(opts) == 1 {
		return opts[0]
	}
	return &Node{kind: nAlt, kids: opts}
}

func (p *parser) seq(depth int) (*Node, error) {
	n := &Node{kind: nSeq}
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch c {
		case '|', ';', ')', ']':
			return n, nil
		case '(', '[':
			p.i++
			stop := byte(')')
			if c == '[' {
				stop = ']'
			}
			in, err := p.alts(stop, depth+1)
			if err != nil {
				return nil, err
			}
			p.i++ // cierre
			if c == '[' {
				in = &Node{kind: nAlt, kids: []*Node{in, {kind: nSeq}}}
			}
			n.kids = append(n.kids, in)
		case '<', '{':
			end := byte('>')
			if c == '{' {
				end = '}'
			}
			j := strings.IndexByte(p.s[p.i:], end)
			if j < 0 {
				return nil, fmt.Errorf("missing %q in %q", end, p.s)
			}
			name := strings.TrimSpace(p.s[p.i+1 : p.i+j])
			p.i += j + 1
			if name == "" {
				return nil, fmt.Errorf("empty reference in %q", p.s)
			}
			if c == '<' {
				n.kids = append(n.kids, &Node{kind: nRule, name: name})
			} else {
				list, slot, ok := strings.Cut(name, ":")
				if !ok {
					slot = list
				}
				n.kids = append(n.kids, &Node{kind: nList, name: list, slot: slot})
			}
		case '>', '}':
			return nil, fmt.Errorf("unexpected %q in %q", c, p.s)
		default:
			j := p.i
			for j < len(p.s) && !strings.ContainsRune("|;()[]<>{}", rune(p.s[j])) {
				j++
			}
			n.kids = append(n.kids, &Node{kind: nText, text: p.s[p.i:j]})
			p.i = j
		}
	}
	return n, nil
}

// ListValue es un valor de una lista: el texto (que puede ser a su vez una
// plantilla, «(naranja|anaranjado)») y el valor que rellena el hueco.
type ListValue struct {
	In  string
	Out string
}

// List es una lista de valores, un rango numérico o un comodín.
type List struct {
	Values   []ListValue
	Range    bool
	From, To int
	Halves   bool // el rango admite medios («21.5»)
	Wildcard bool
}

// Capture es un hueco rellenado al expandir: qué lista, qué hueco, qué valor
// y dónde quedó en el texto final.
type Capture struct {
	List, Slot string
	Out        string
	Num        float64
	IsNum      bool
	Start, End int
}

// Expansion es una frase generada con sus huecos.
type Expansion struct {
	Text     string
	Captures []Capture
}

// Grammar reúne reglas y listas de un idioma.
type Grammar struct {
	Lang  string
	Rules map[string]string
	Lists map[string]*List
	// Filler da valores para los comodines ({search_query}…); nil = sin comodines.
	Filler func(list string) []string

	parsed map[string]*Node
}

// ErrUnknown: la plantilla usa una regla o lista que la gramática no tiene.
var ErrUnknown = errors.New("unknown rule or list")

func (g *Grammar) rule(name string) (*Node, error) {
	if n, ok := g.parsed[name]; ok {
		return n, nil
	}
	raw, ok := g.Rules[name]
	if !ok {
		return nil, fmt.Errorf("%w: <%s>", ErrUnknown, name)
	}
	if g.parsed == nil {
		g.parsed = map[string]*Node{}
	}
	g.parsed[name] = nil // detecta recursión: una regla no puede usarse a sí misma
	n, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	g.parsed[name] = n
	return n, nil
}

func (g *Grammar) value(v string) (*Node, error) {
	key := "\x00" + v
	if n, ok := g.parsed[key]; ok {
		return n, nil
	}
	n, err := Parse(v)
	if err != nil {
		return nil, err
	}
	if g.parsed == nil {
		g.parsed = map[string]*Node{}
	}
	g.parsed[key] = n
	return n, nil
}

// Check comprueba que todas las reglas y listas que usa n existen.
func (g *Grammar) Check(n *Node) error { _, err := g.countOf(n, 0); return err }

// countOf: número (saturado) de frases distintas que genera n. Sirve para
// muestrear alternativas en proporción a lo que generan: así las frases salen
// casi uniformes en vez de sobrerrepresentar la rama corta.
func (g *Grammar) countOf(n *Node, depth int) (float64, error) {
	if depth > 64 {
		return 0, errors.New("rules nested too deep (recursive?)")
	}
	const sat = 1e12
	switch n.kind {
	case nText:
		return 1, nil
	case nSeq, nPerm:
		c := 1.0
		for _, k := range n.kids {
			x, err := g.countOf(k, depth+1)
			if err != nil {
				return 0, err
			}
			c = min(c*x, sat)
		}
		if n.kind == nPerm {
			for i := 2; i <= len(n.kids); i++ {
				c = min(c*float64(i), sat)
			}
		}
		return c, nil
	case nAlt:
		c := 0.0
		for _, k := range n.kids {
			x, err := g.countOf(k, depth+1)
			if err != nil {
				return 0, err
			}
			c = min(c+x, sat)
		}
		return c, nil
	case nRule:
		r, err := g.rule(n.name)
		if err != nil {
			return 0, err
		}
		if r == nil {
			return 0, fmt.Errorf("recursive rule <%s>", n.name)
		}
		return g.countOf(r, depth+1)
	case nList:
		l, ok := g.Lists[n.name]
		if !ok {
			return 0, fmt.Errorf("%w: {%s}", ErrUnknown, n.name)
		}
		switch {
		case l.Wildcard:
			if g.Filler == nil || len(g.Filler(n.name)) == 0 {
				return 0, fmt.Errorf("%w: wildcard {%s}", ErrUnknown, n.name)
			}
			return float64(len(g.Filler(n.name))), nil
		case l.Range:
			return float64(l.To - l.From + 1), nil
		}
		if len(l.Values) == 0 {
			return 0, fmt.Errorf("%w: empty list {%s}", ErrUnknown, n.name)
		}
		return float64(len(l.Values)), nil
	}
	return 1, nil
}

// Rand es la fuente de aleatoriedad del muestreo (splitmix64: determinista y
// igual en todas partes).
type Rand struct{ S uint64 }

// Next devuelve el siguiente número.
func (r *Rand) Next() uint64 {
	r.S += 0x9e3779b97f4a7c15
	z := r.S
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// Float devuelve un número en [0, 1).
func (r *Rand) Float() float64 { return float64(r.Next()>>11) / (1 << 53) }

// Intn devuelve un número en [0, n).
func (r *Rand) Intn(n int) int { return int(r.Next() % uint64(n)) }

const (
	markOpen  = '\x01'
	markClose = '\x02'
)

type sampler struct {
	g    *Grammar
	rng  *Rand
	b    strings.Builder
	caps []Capture
}

// Sample genera una frase de n al azar (determinista dada la semilla).
func (g *Grammar) Sample(n *Node, rng *Rand) (Expansion, error) {
	if err := g.Check(n); err != nil {
		return Expansion{}, err
	}
	s := &sampler{g: g, rng: rng}
	if err := s.walk(n, 0); err != nil {
		return Expansion{}, err
	}
	return finish(s.b.String(), s.caps), nil
}

func (s *sampler) walk(n *Node, depth int) error {
	switch n.kind {
	case nText:
		s.b.WriteString(n.text)
	case nSeq:
		for _, k := range n.kids {
			if err := s.walk(k, depth+1); err != nil {
				return err
			}
		}
	case nPerm:
		order := make([]int, len(n.kids))
		for i := range order {
			order[i] = i
		}
		for i := len(order) - 1; i > 0; i-- {
			j := s.rng.Intn(i + 1)
			order[i], order[j] = order[j], order[i]
		}
		for i, k := range order {
			if i > 0 {
				s.b.WriteByte(' ')
			}
			if err := s.walk(n.kids[k], depth+1); err != nil {
				return err
			}
		}
	case nAlt:
		total := 0.0
		w := make([]float64, len(n.kids))
		for i, k := range n.kids {
			w[i], _ = s.g.countOf(k, depth)
			total += w[i]
		}
		x := s.rng.Float() * total
		pick := len(n.kids) - 1
		for i := range w {
			if x < w[i] {
				pick = i
				break
			}
			x -= w[i]
		}
		return s.walk(n.kids[pick], depth+1)
	case nRule:
		r, err := s.g.rule(n.name)
		if err != nil {
			return err
		}
		return s.walk(r, depth+1)
	case nList:
		return s.list(n, depth)
	}
	return nil
}

func (s *sampler) list(n *Node, depth int) error {
	l := s.g.Lists[n.name]
	c := Capture{List: n.name, Slot: n.slot}
	s.b.WriteByte(markOpen)
	switch {
	case l.Wildcard:
		f := s.g.Filler(n.name)
		v := f[s.rng.Intn(len(f))]
		c.Out = v
		s.b.WriteString(v)
	case l.Range:
		v := float64(l.From + s.rng.Intn(l.To-l.From+1))
		if l.Halves && v < float64(l.To) && s.rng.Intn(8) == 0 {
			v += 0.5
		}
		c.Num, c.IsNum, c.Out = v, true, FormatValue(v)
		switch {
		case v == float64(int(v)) && v <= 100 && s.rng.Intn(3) == 0:
			s.b.WriteString(NumberWords(int(v), s.g.Lang))
		case s.g.Lang == "es" && v != float64(int(v)) && s.rng.Intn(2) == 0:
			s.b.WriteString(strings.Replace(FormatValue(v), ".", ",", 1))
		default:
			s.b.WriteString(FormatValue(v))
		}
	default:
		lv := l.Values[s.rng.Intn(len(l.Values))]
		c.Out = lv.Out
		in, err := s.g.value(lv.In)
		if err != nil {
			return err
		}
		if err := s.walk(in, depth+1); err != nil {
			return err
		}
	}
	s.b.WriteByte(markClose)
	s.caps = append(s.caps, c)
	return nil
}

// finish colapsa los espacios y convierte las marcas de hueco en posiciones.
func finish(raw string, caps []Capture) Expansion {
	var b strings.Builder
	ci := 0
	space := false
	var open []int
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch c {
		case markOpen:
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			open = append(open, ci)
			caps[ci].Start = b.Len()
			ci++
			continue
		case markClose:
			k := open[len(open)-1]
			open = open[:len(open)-1]
			caps[k].End = b.Len()
			continue
		case ' ', '\t', '\n':
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteByte(c)
	}
	out := Expansion{Text: b.String()}
	for _, c := range caps {
		// Un hueco que quedó vacío (lista con valor vacío) no es un hueco.
		if c.End > c.Start {
			// Los espacios del borde no se incluyen.
			for c.Start < c.End && out.Text[c.Start] == ' ' {
				c.Start++
			}
			for c.End > c.Start && out.Text[c.End-1] == ' ' {
				c.End--
			}
			out.Captures = append(out.Captures, c)
		}
	}
	return out
}

// Enumerate genera todas las frases de n, hasta limit (si hay más, error).
// Las listas se sustituyen por listText(nombre): el emparejador las
// representa con un marcador («zzarea») que luego rellena al emparejar.
func (g *Grammar) Enumerate(n *Node, limit int, listText func(list, slot string) string) ([]string, error) {
	c, err := g.countOfWith(n, listText)
	if err != nil {
		return nil, err
	}
	if c > float64(limit) {
		return nil, fmt.Errorf("template expands to %.0f sentences (limit %d)", c, limit)
	}
	outs, err := g.enum(n, listText, 0)
	if err != nil {
		return nil, err
	}
	res := make([]string, 0, len(outs))
	seen := map[string]bool{}
	for _, o := range outs {
		t := strings.Join(strings.Fields(o), " ")
		if !seen[t] {
			seen[t] = true
			res = append(res, t)
		}
	}
	return res, nil
}

func (g *Grammar) countOfWith(n *Node, listText func(list, slot string) string) (float64, error) {
	if listText == nil {
		return g.countOf(n, 0)
	}
	// Con listas como marcadores cada lista cuenta 1: se enumera la estructura.
	saved := g.Lists
	g.Lists = map[string]*List{}
	defer func() { g.Lists = saved }()
	var collect func(*Node, int) error
	collect = func(m *Node, d int) error {
		if d > 64 {
			return errors.New("rules nested too deep")
		}
		switch m.kind {
		case nList:
			g.Lists[m.name] = &List{Values: []ListValue{{In: "x"}}}
		case nRule:
			r, err := g.rule(m.name)
			if err != nil {
				return err
			}
			if r == nil {
				return fmt.Errorf("recursive rule <%s>", m.name)
			}
			return collect(r, d+1)
		}
		for _, k := range m.kids {
			if err := collect(k, d+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := collect(n, 0); err != nil {
		return 0, err
	}
	return g.countOf(n, 0)
}

func (g *Grammar) enum(n *Node, listText func(list, slot string) string, depth int) ([]string, error) {
	switch n.kind {
	case nText:
		return []string{n.text}, nil
	case nSeq:
		acc := []string{""}
		for _, k := range n.kids {
			xs, err := g.enum(k, listText, depth+1)
			if err != nil {
				return nil, err
			}
			acc = cross(acc, xs, "")
		}
		return acc, nil
	case nPerm:
		var out []string
		permute(len(n.kids), func(order []int) {
			acc := []string{""}
			for i, k := range order {
				xs, _ := g.enum(n.kids[k], listText, depth+1)
				sep := ""
				if i > 0 {
					sep = " "
				}
				acc = cross(acc, xs, sep)
			}
			out = append(out, acc...)
		})
		return out, nil
	case nAlt:
		var out []string
		for _, k := range n.kids {
			xs, err := g.enum(k, listText, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, xs...)
		}
		return out, nil
	case nRule:
		r, err := g.rule(n.name)
		if err != nil {
			return nil, err
		}
		return g.enum(r, listText, depth+1)
	case nList:
		return []string{" " + listText(n.name, n.slot) + " "}, nil
	}
	return nil, nil
}

func cross(a, b []string, sep string) []string {
	out := make([]string, 0, len(a)*len(b))
	for _, x := range a {
		for _, y := range b {
			out = append(out, x+sep+y)
		}
	}
	return out
}

func permute(n int, f func([]int)) {
	p := make([]int, n)
	for i := range p {
		p[i] = i
	}
	var rec func(int)
	rec = func(k int) {
		if k == n {
			f(p)
			return
		}
		for i := k; i < n; i++ {
			p[k], p[i] = p[i], p[k]
			rec(k + 1)
			p[k], p[i] = p[i], p[k]
		}
	}
	rec(0)
}
