package main

// Un subconjunto mínimo de YAML para los ficheros de grafo, sin dependencias
// (el núcleo no tiene ninguna). Lo que entiende es lo que usa un grafo:
//
//   - mapas y listas por sangría (espacios, nunca tabuladores), con "- clave:
//     valor" como primer elemento de un mapa dentro de una lista;
//   - colecciones en línea {a: b, c: [1, 2]} y [x, y], anidadas, en una línea;
//   - escalares planos, "dobles" (\" \\ \n \t) y 'simples' ('' es una comilla);
//   - comentarios con # (al principio o tras un espacio, fuera de comillas).
//
// Un escalar plano que es un entero se lee como número; true y false como
// booleanos; null y ~ como nulo; todo lo demás, texto. Lo que no entiende
// (anclas, etiquetas, bloques | y >, varios documentos, claves complejas) es
// un error con su línea, nunca una interpretación a medias. El resultado va
// después por encoding/json con DisallowUnknownFields, así que un campo mal
// escrito se detecta igual que en JSON.

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// lineaYAML es una línea con contenido: su número, su sangría y el texto sin
// sangría ni comentario.
type lineaYAML struct {
	num    int
	indent int
	texto  string
}

type lectorYAML struct {
	ls []lineaYAML
	i  int
}

// parseYAML convierte el subconjunto a map[string]any / []any / escalares.
func parseYAML(datos []byte) (any, error) {
	var ls []lineaYAML
	for n, raw := range strings.Split(strings.ReplaceAll(string(datos), "\r\n", "\n"), "\n") {
		sinSangria := strings.TrimLeft(raw, " ")
		if strings.HasPrefix(sinSangria, "\t") {
			return nil, fmt.Errorf("line %d: tabs are not valid indentation in YAML", n+1)
		}
		texto, err := quitarComentario(sinSangria)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n+1, err)
		}
		texto = strings.TrimRight(texto, " \t")
		if texto == "" {
			continue
		}
		if texto == "---" && len(ls) == 0 {
			continue
		}
		if texto == "---" || texto == "..." {
			return nil, fmt.Errorf("line %d: only one YAML document per file", n+1)
		}
		ls = append(ls, lineaYAML{num: n + 1, indent: len(raw) - len(sinSangria), texto: texto})
	}
	if len(ls) == 0 {
		return nil, errors.New("empty file")
	}
	l := &lectorYAML{ls: ls}
	v, err := l.bloque(ls[0].indent)
	if err != nil {
		return nil, err
	}
	if l.i < len(l.ls) {
		return nil, fmt.Errorf("line %d: unexpected indentation", l.ls[l.i].num)
	}
	return v, nil
}

// quitarComentario corta el # de un comentario fuera de comillas.
func quitarComentario(s string) (string, error) {
	var comilla byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case comilla == '"' && c == '\\':
			i++
		case comilla != 0:
			if c == comilla {
				if comilla == '\'' && i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				comilla = 0
			}
		case c == '"' || c == '\'':
			// Solo abre si empieza un escalar (tras espacio, ':', '-', '[', '{', ',' o al principio).
			if i == 0 || strings.IndexByte(" :-[{,", s[i-1]) >= 0 {
				comilla = c
			}
		case c == '#' && (i == 0 || s[i-1] == ' '):
			return s[:i], nil
		}
	}
	if comilla != 0 {
		return "", errors.New("unterminated quoted string")
	}
	return s, nil
}

// esElementoLista dice si el texto es "- algo" o "-".
func esElementoLista(t string) bool { return t == "-" || strings.HasPrefix(t, "- ") }

// bloque lee el mapa o la lista que empieza en la línea actual, con sangría
// indent.
func (l *lectorYAML) bloque(indent int) (any, error) {
	if l.i >= len(l.ls) {
		return nil, nil
	}
	ln := l.ls[l.i]
	if ln.indent != indent {
		return nil, fmt.Errorf("line %d: unexpected indentation", ln.num)
	}
	if esElementoLista(ln.texto) {
		return l.lista(indent)
	}
	return l.mapa(indent)
}

func (l *lectorYAML) lista(indent int) ([]any, error) {
	out := []any{}
	for l.i < len(l.ls) {
		ln := l.ls[l.i]
		if ln.indent < indent || (ln.indent == indent && !esElementoLista(ln.texto)) {
			break
		}
		if ln.indent > indent {
			return nil, fmt.Errorf("line %d: unexpected indentation", ln.num)
		}
		resto := strings.TrimLeft(strings.TrimPrefix(ln.texto, "-"), " ")
		switch {
		case resto == "":
			l.i++
			if l.i < len(l.ls) && l.ls[l.i].indent > indent {
				v, err := l.bloque(l.ls[l.i].indent)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			} else {
				out = append(out, nil)
			}
		case esElementoLista(resto) || pareceClave(resto):
			// "- clave: valor" o "- - x": un mapa (o una lista) cuya sangría
			// es la columna de lo que sigue al guion.
			col := ln.indent + (len(ln.texto) - len(resto))
			l.ls[l.i] = lineaYAML{num: ln.num, indent: col, texto: resto}
			v, err := l.bloque(col)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		default:
			v, err := valorEnLinea(resto, ln.num)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			l.i++
		}
	}
	return out, nil
}

func (l *lectorYAML) mapa(indent int) (map[string]any, error) {
	out := map[string]any{}
	for l.i < len(l.ls) {
		ln := l.ls[l.i]
		if ln.indent < indent {
			break
		}
		if ln.indent > indent {
			return nil, fmt.Errorf("line %d: unexpected indentation", ln.num)
		}
		if esElementoLista(ln.texto) {
			return nil, fmt.Errorf("line %d: a list item where a key was expected", ln.num)
		}
		clave, resto, err := partirClave(ln.texto, ln.num)
		if err != nil {
			return nil, err
		}
		if _, dup := out[clave]; dup {
			return nil, fmt.Errorf("line %d: key %q is repeated", ln.num, clave)
		}
		l.i++
		if resto != "" {
			v, err := valorEnLinea(resto, ln.num)
			if err != nil {
				return nil, err
			}
			out[clave] = v
			continue
		}
		switch {
		case l.i < len(l.ls) && l.ls[l.i].indent > indent:
			v, err := l.bloque(l.ls[l.i].indent)
			if err != nil {
				return nil, err
			}
			out[clave] = v
		case l.i < len(l.ls) && l.ls[l.i].indent == indent && esElementoLista(l.ls[l.i].texto):
			// "clave:\n- a\n- b" a la misma sangría también es una lista.
			v, err := l.lista(indent)
			if err != nil {
				return nil, err
			}
			out[clave] = v
		default:
			out[clave] = nil
		}
	}
	return out, nil
}

// pareceClave dice si el texto empieza por "clave:" (y no es un escalar o
// una colección en línea).
func pareceClave(t string) bool {
	if t == "" || strings.IndexByte("[{", t[0]) >= 0 {
		return false
	}
	_, _, err := partirClave(t, 0)
	return err == nil
}

// partirClave separa "clave: resto".
func partirClave(t string, num int) (clave, resto string, err error) {
	if t[0] == '"' || t[0] == '\'' {
		s, n, err := leerComillas(t, 0)
		if err != nil {
			return "", "", fmt.Errorf("line %d: %w", num, err)
		}
		r := strings.TrimLeft(t[n:], " ")
		if !strings.HasPrefix(r, ":") || (len(r) > 1 && r[1] != ' ') {
			return "", "", fmt.Errorf("line %d: expected ':' after the key", num)
		}
		return s, strings.TrimSpace(r[1:]), nil
	}
	for i := 0; i < len(t); i++ {
		if t[i] == ':' && (i+1 == len(t) || t[i+1] == ' ') {
			clave = strings.TrimSpace(t[:i])
			if clave == "" || strings.ContainsAny(clave, "{}[],&*!|>") || strings.HasPrefix(clave, "? ") {
				return "", "", fmt.Errorf("line %d: unsupported key %q", num, clave)
			}
			return clave, strings.TrimSpace(t[i+1:]), nil
		}
	}
	return "", "", fmt.Errorf("line %d: expected 'key: value'", num)
}

// valorEnLinea interpreta el valor de una línea: una colección en línea o un
// escalar.
func valorEnLinea(t string, num int) (any, error) {
	switch t[0] {
	case '|', '>':
		return nil, fmt.Errorf("line %d: block scalars (| and >) are not supported; quote the text", num)
	case '&', '*', '!':
		return nil, fmt.Errorf("line %d: anchors, aliases and tags are not supported", num)
	}
	p := &flujoYAML{s: t, num: num}
	v, err := p.valor()
	if err != nil {
		return nil, err
	}
	p.espacios()
	if p.i != len(p.s) {
		return nil, fmt.Errorf("line %d: unexpected %q", num, p.s[p.i:])
	}
	return v, nil
}

// flujoYAML lee colecciones en línea.
type flujoYAML struct {
	s   string
	i   int
	num int
	// dentro: estamos dentro de [] o {}: ',' ']' '}' terminan un escalar.
	dentro int
}

func (p *flujoYAML) espacios() {
	for p.i < len(p.s) && p.s[p.i] == ' ' {
		p.i++
	}
}

func (p *flujoYAML) errorf(format string, a ...any) error {
	return fmt.Errorf("line %d: "+format, append([]any{p.num}, a...)...)
}

func (p *flujoYAML) valor() (any, error) {
	p.espacios()
	if p.i >= len(p.s) {
		return nil, p.errorf("missing value")
	}
	switch p.s[p.i] {
	case '[':
		return p.lista()
	case '{':
		return p.mapa()
	case '"', '\'':
		s, n, err := leerComillas(p.s, p.i)
		if err != nil {
			return nil, p.errorf("%v", err)
		}
		p.i = n
		return s, nil
	case '&', '*', '!':
		return nil, p.errorf("anchors, aliases and tags are not supported")
	}
	ini := p.i
	for p.i < len(p.s) {
		c := p.s[p.i]
		if p.dentro > 0 && (c == ',' || c == ']' || c == '}') {
			break
		}
		if p.dentro > 0 && c == ':' && (p.i+1 == len(p.s) || p.s[p.i+1] == ' ') {
			break
		}
		p.i++
	}
	return escalarPlano(strings.TrimSpace(p.s[ini:p.i])), nil
}

func (p *flujoYAML) lista() ([]any, error) {
	p.i++ // [
	p.dentro++
	defer func() { p.dentro-- }()
	out := []any{}
	for {
		p.espacios()
		if p.i >= len(p.s) {
			return nil, p.errorf("unterminated [ (flow lists must fit on one line)")
		}
		if p.s[p.i] == ']' {
			p.i++
			return out, nil
		}
		v, err := p.valor()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.espacios()
		if p.i < len(p.s) && p.s[p.i] == ',' {
			p.i++
			continue
		}
		if p.i < len(p.s) && p.s[p.i] == ']' {
			continue
		}
		return nil, p.errorf("expected ',' or ']'")
	}
}

func (p *flujoYAML) mapa() (map[string]any, error) {
	p.i++ // {
	p.dentro++
	defer func() { p.dentro-- }()
	out := map[string]any{}
	for {
		p.espacios()
		if p.i >= len(p.s) {
			return nil, p.errorf("unterminated { (flow maps must fit on one line)")
		}
		if p.s[p.i] == '}' {
			p.i++
			return out, nil
		}
		k, err := p.valor()
		if err != nil {
			return nil, err
		}
		clave, ok := k.(string)
		if !ok {
			clave = fmt.Sprint(k)
		}
		if clave == "" {
			return nil, p.errorf("empty key")
		}
		p.espacios()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return nil, p.errorf("expected ':' after key %q", clave)
		}
		p.i++
		v, err := p.valor()
		if err != nil {
			return nil, err
		}
		if _, dup := out[clave]; dup {
			return nil, p.errorf("key %q is repeated", clave)
		}
		out[clave] = v
		p.espacios()
		if p.i < len(p.s) && p.s[p.i] == ',' {
			p.i++
			continue
		}
		if p.i < len(p.s) && p.s[p.i] == '}' {
			continue
		}
		return nil, p.errorf("expected ',' or '}'")
	}
}

// leerComillas lee el escalar entre comillas que empieza en s[i] y devuelve
// su valor y la posición siguiente.
func leerComillas(s string, i int) (string, int, error) {
	q := s[i]
	var b strings.Builder
	for j := i + 1; j < len(s); j++ {
		c := s[j]
		switch {
		case q == '\'' && c == '\'':
			if j+1 < len(s) && s[j+1] == '\'' {
				b.WriteByte('\'')
				j++
				continue
			}
			return b.String(), j + 1, nil
		case q == '"' && c == '"':
			return b.String(), j + 1, nil
		case q == '"' && c == '\\':
			if j+1 >= len(s) {
				return "", 0, errors.New("unterminated escape")
			}
			j++
			switch s[j] {
			case '"', '\\', '/':
				b.WriteByte(s[j])
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				return "", 0, fmt.Errorf("unsupported escape \\%c", s[j])
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", 0, errors.New("unterminated quoted string")
}

// escalarPlano interpreta un escalar sin comillas.
func escalarPlano(s string) any {
	switch s {
	case "", "null", "~", "Null", "NULL":
		return nil
	case "true", "True", "TRUE":
		return true
	case "false", "False", "FALSE":
		return false
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	return s
}
