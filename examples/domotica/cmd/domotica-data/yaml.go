package main

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Un analizador del subconjunto de YAML que usan los ficheros de
// home-assistant/intents: mapas y listas por sangría, escalares planos y entre
// comillas, bloques «|» y «>», listas en línea sencillas y varios documentos
// separados por «---». No es un YAML general, y no hace falta: el go.mod raíz
// no tiene dependencias y estos ficheros son regulares. Lo que no entiende lo
// devuelve como error y el fichero se cuenta como omitido.

type yline struct {
	indent int
	text   string // sin sangría ni comentario
	num    int
}

func stripComment(s string) string {
	inD, inS := false, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' && !inS:
			if i == 0 || s[i-1] != '\\' {
				inD = !inD
			}
		case c == '\'' && !inD:
			inS = !inS
		case c == '#' && !inD && !inS && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return strings.TrimRight(s[:i], " \t")
		}
	}
	return strings.TrimRight(s, " \t\r")
}

// parseYAMLDocs devuelve un valor por documento.
func parseYAMLDocs(src string) ([]any, error) {
	var docs []any
	var cur []string
	flush := func() error {
		if len(cur) == 0 {
			return nil
		}
		v, err := parseYAML(cur)
		if err != nil {
			return err
		}
		if v != nil {
			docs = append(docs, v)
		}
		cur = nil
		return nil
	}
	for _, l := range strings.Split(src, "\n") {
		if strings.TrimRight(l, " \r") == "---" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		cur = append(cur, l)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return docs, nil
}

func parseYAML(raw []string) (any, error) {
	p := &yparser{raw: raw}
	for i, l := range raw {
		if strings.HasPrefix(strings.TrimLeft(l, " "), "\t") {
			return nil, fmt.Errorf("line %d: tab indentation", i+1)
		}
		t := strings.TrimLeft(l, " ")
		ind := len(l) - len(t)
		t = stripComment(t)
		p.all = append(p.all, yline{indent: ind, text: t, num: i})
	}
	// Las líneas vacías o solo comentario no cuentan, salvo dentro de bloques
	// «|», que se leen de p.all directamente.
	for i, l := range p.all {
		if l.text != "" {
			p.idx = append(p.idx, i)
		}
	}
	if len(p.idx) == 0 {
		return nil, nil
	}
	v, err := p.block(p.all[p.idx[0]].indent)
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.idx) {
		return nil, fmt.Errorf("line %d: unexpected indentation", p.all[p.idx[p.pos]].num+1)
	}
	return v, nil
}

type yparser struct {
	raw []string
	all []yline
	idx []int // índices de p.all con contenido
	pos int
}

func (p *yparser) peek() (yline, bool) {
	if p.pos >= len(p.idx) {
		return yline{}, false
	}
	return p.all[p.idx[p.pos]], true
}

func isSeqItem(t string) bool { return t == "-" || strings.HasPrefix(t, "- ") }

func (p *yparser) block(indent int) (any, error) {
	l, ok := p.peek()
	if !ok {
		return nil, nil
	}
	if isSeqItem(l.text) {
		return p.seq(indent)
	}
	return p.mapping(indent)
}

func (p *yparser) seq(indent int) (any, error) {
	var out []any
	for {
		l, ok := p.peek()
		if !ok || l.indent != indent || !isSeqItem(l.text) {
			return out, nil
		}
		rest := strings.TrimSpace(strings.TrimPrefix(l.text, "-"))
		if rest == "" {
			p.pos++
			nl, ok := p.peek()
			if !ok || nl.indent <= indent {
				out = append(out, nil)
				continue
			}
			v, err := p.block(nl.indent)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			continue
		}
		if _, _, isKey := splitKey(rest); isKey {
			// «- clave: valor»: un mapa cuya primera clave va en la línea del
			// guion; el resto de claves van sangradas como esa clave.
			sub := indent + (len(l.text) - len(rest))
			p.all[p.idx[p.pos]] = yline{indent: sub, text: rest, num: l.num}
			v, err := p.mapping(sub)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			continue
		}
		p.pos++
		v, err := p.scalarOrBlock(rest, indent, l)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}

// splitKey separa «clave: resto». La clave es un identificador o va entre comillas.
func splitKey(t string) (key, rest string, ok bool) {
	if t == "" {
		return "", "", false
	}
	if t[0] == '"' || t[0] == '\'' {
		q := t[0]
		j := strings.IndexByte(t[1:], q)
		if j < 0 {
			return "", "", false
		}
		after := t[j+2:]
		if after == ":" || strings.HasPrefix(after, ": ") {
			return t[1 : j+1], strings.TrimSpace(strings.TrimPrefix(after, ":")), true
		}
		return "", "", false
	}
	i := strings.Index(t, ":")
	if i <= 0 || (i+1 < len(t) && t[i+1] != ' ') {
		return "", "", false
	}
	k := t[:i]
	for _, c := range k {
		if !(c == '_' || c == '-' || c == '.' || c == ' ' || unicode.IsLetter(c) || unicode.IsDigit(c)) {
			return "", "", false
		}
	}
	return k, strings.TrimSpace(t[i+1:]), true
}

func (p *yparser) mapping(indent int) (any, error) {
	out := map[string]any{}
	for {
		l, ok := p.peek()
		if !ok || l.indent < indent {
			return out, nil
		}
		if l.indent > indent {
			return nil, fmt.Errorf("line %d: unexpected indentation", l.num+1)
		}
		if isSeqItem(l.text) {
			return out, nil // la lista pertenece a la clave de arriba
		}
		k, rest, ok := splitKey(l.text)
		if !ok {
			return nil, fmt.Errorf("line %d: expected key: value", l.num+1)
		}
		p.pos++
		if rest == "" {
			nl, ok := p.peek()
			switch {
			case ok && nl.indent > indent:
				v, err := p.block(nl.indent)
				if err != nil {
					return nil, err
				}
				out[k] = v
			case ok && nl.indent == indent && isSeqItem(nl.text):
				v, err := p.seq(indent)
				if err != nil {
					return nil, err
				}
				out[k] = v
			default:
				out[k] = nil
			}
			continue
		}
		v, err := p.scalarOrBlock(rest, indent, l)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
}

// scalarOrBlock interpreta un valor en línea o, si es «|»/«>», el bloque de
// líneas más sangradas que le sigue.
func (p *yparser) scalarOrBlock(rest string, indent int, l yline) (any, error) {
	if rest[0] == '|' || rest[0] == '>' {
		var lines []string
		start := p.idx[p.pos-1] + 1
		end := start
		for end < len(p.all) {
			al := p.all[end]
			if al.text != "" && al.indent <= indent {
				break
			}
			end++
		}
		for i := start; i < end; i++ {
			lines = append(lines, strings.TrimSpace(p.raw[i]))
		}
		for p.pos < len(p.idx) && p.idx[p.pos] < end {
			p.pos++
		}
		sep := "\n"
		if rest[0] == '>' {
			sep = " "
		}
		return strings.TrimSpace(strings.Join(lines, sep)), nil
	}
	return scalar(rest, l.num)
}

func scalar(s string, num int) (any, error) {
	switch s[0] {
	case '"':
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			c := s[i]
			if c == '\\' && i+1 < len(s) {
				i++
				switch s[i] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				default:
					b.WriteByte(s[i])
				}
				continue
			}
			if c == '"' {
				if strings.TrimSpace(s[i+1:]) != "" {
					return nil, fmt.Errorf("line %d: text after closing quote", num+1)
				}
				return b.String(), nil
			}
			b.WriteByte(c)
		}
		return nil, fmt.Errorf("line %d: unterminated string", num+1)
	case '\'':
		body := s[1:]
		var b strings.Builder
		for i := 0; i < len(body); i++ {
			if body[i] == '\'' {
				if i+1 < len(body) && body[i+1] == '\'' {
					b.WriteByte('\'')
					i++
					continue
				}
				return b.String(), nil
			}
			b.WriteByte(body[i])
		}
		return nil, fmt.Errorf("line %d: unterminated string", num+1)
	case '[':
		// Lista en línea sencilla: «[a, b]». Una plantilla sin comillas que
		// empieza por «[» no es YAML válido; si no parece una lista, error.
		if !strings.HasSuffix(s, "]") {
			return nil, fmt.Errorf("line %d: unsupported flow value", num+1)
		}
		inner := strings.TrimSpace(s[1 : len(s)-1])
		if inner == "" {
			return []any{}, nil
		}
		var out []any
		for _, part := range strings.Split(inner, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			v, err := scalar(part, num)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case '{':
		if s == "{}" {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("line %d: flow mappings are not supported", num+1)
	case '&', '*', '!':
		return nil, errors.New("anchors, aliases and tags are not supported")
	}
	return s, nil
}

// Accesores tolerantes para recorrer lo analizado.
func ymap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func ylist(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case nil:
		return nil
	}
	return []any{v}
}

func ystr(v any) string {
	s, _ := v.(string)
	return s
}
