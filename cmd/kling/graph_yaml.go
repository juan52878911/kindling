package main

// Un subconjunto mínimo de YAML para los ficheros de grafo, sin dependencias
// (el núcleo no tiene ninguna). Lo que entiende es lo que usa un grafo:
//
//   - mapas y listas por sangría (espacios, nunca tabuladores), con "- clave:
//     valor" como primer elemento de un mapa dentro de una lista;
//   - colecciones en línea {a: b, c: [1, 2]} y [x, y], anidadas, en una línea;
//   - escalares planos, "dobles" (\" \\ \n \t) y 'simples' ('' es una comilla);
//   - escalares de bloque literales (|) y plegados (>) como en YAML 1.2, con
//     chomping (- +) e indicador de sangría (|2); su contenido es texto tal
//     cual, sin comillas ni comentarios;
//   - comentarios con # (al principio o tras un espacio, fuera de comillas).
//
// Un escalar plano que es un entero se lee como número; true y false como
// booleanos; null y ~ como nulo; todo lo demás, texto. Lo que no entiende
// (anclas, etiquetas, varios documentos, claves complejas) es
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
	// bloques: el valor de cada escalar de bloque, por la línea de su cabecera.
	bloques map[int]string
}

// parseYAML convierte el subconjunto a map[string]any / []any / escalares.
func parseYAML(datos []byte) (any, error) {
	src := strings.ReplaceAll(string(datos), "\r\n", "\n")
	raws := strings.Split(src, "\n")
	// Con salto final, Split deja un "" que no es una línea; sin él, la
	// última línea no tiene salto (importa al cortar un bloque al final).
	saltoFinal := strings.HasSuffix(src, "\n")
	if saltoFinal {
		raws = raws[:len(raws)-1]
	}
	var ls []lineaYAML
	bloques := map[int]string{}
	for n := 0; n < len(raws); n++ {
		raw := raws[n]
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
		indent := len(raw) - len(sinSangria)
		ls = append(ls, lineaYAML{num: n + 1, indent: indent, texto: texto})
		if padre, cab, ok := cabeceraBloque(texto, indent, len(ls) == 1); ok {
			v, sig, err := leerBloque(raws, n+1, padre, cab, saltoFinal)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", n+1, err)
			}
			bloques[n+1] = v
			n = sig - 1
		}
	}
	if len(ls) == 0 {
		return nil, errors.New("empty file")
	}
	l := &lectorYAML{ls: ls, bloques: bloques}
	if v, ok := bloques[ls[0].num]; ok && len(ls) == 1 && strings.IndexByte("|>", ls[0].texto[0]) >= 0 {
		return v, nil // el documento entero es un bloque
	}
	v, err := l.bloque(ls[0].indent)
	if err != nil {
		return nil, err
	}
	if l.i < len(l.ls) {
		return nil, fmt.Errorf("line %d: unexpected indentation", l.ls[l.i].num)
	}
	return v, nil
}

// cabeceraBloque dice si la línea termina en la cabecera de un escalar de
// bloque (| o >, con sus indicadores) y devuelve la sangría del nodo padre:
// la columna de la clave en "clave: |", la del guion en "- |", y -1 si el
// documento entero es el bloque. Los guiones de "- - clave: |" se recorren.
func cabeceraBloque(t string, col int, primera bool) (padre int, cab string, ok bool) {
	if primera && esCabeceraBloque(t) {
		return -1, t, true
	}
	for {
		switch {
		case esElementoLista(t):
			resto := strings.TrimLeft(strings.TrimPrefix(t, "-"), " ")
			if esCabeceraBloque(resto) {
				return col, resto, true
			}
			col += len(t) - len(resto)
			t = resto
		case pareceClave(t):
			_, resto, _ := partirClave(t, 0)
			if esCabeceraBloque(resto) {
				return col, resto, true
			}
			return 0, "", false
		default:
			return 0, "", false
		}
	}
}

// esCabeceraBloque reconoce "|" o ">" seguidos de, como mucho, un indicador
// de sangría (1-9) y uno de chomping (- o +), en cualquier orden.
func esCabeceraBloque(t string) bool {
	if t == "" || (t[0] != '|' && t[0] != '>') || len(t) > 3 {
		return false
	}
	var dig, chomp bool
	for _, c := range t[1:] {
		switch {
		case c >= '1' && c <= '9' && !dig:
			dig = true
		case (c == '-' || c == '+') && !chomp:
			chomp = true
		default:
			return false
		}
	}
	return true
}

// leerBloque lee el contenido de un escalar de bloque (YAML 1.2, capítulo 8)
// desde raws[ini] y devuelve su valor y la primera línea que ya no es suya.
// El contenido es texto literal: ni comillas ni # se interpretan. padre es la
// sangría del nodo padre; el contenido va más sangrado, y el bloque acaba en
// la primera línea con texto menos sangrada que él.
func leerBloque(raws []string, ini, padre int, cab string, saltoFinal bool) (string, int, error) {
	plegado := cab[0] == '>'
	var chomp byte
	n, explicita := 0, false
	for i := 1; i < len(cab); i++ {
		if c := cab[i]; c == '-' || c == '+' {
			chomp = c
		} else {
			n, explicita = padre+int(c-'0'), true
		}
	}
	espacios := func(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }
	vacia := func(s string) bool { return strings.TrimLeft(s, " ") == "" }
	if !explicita {
		// Sangría autodetectada: la de la primera línea con texto. Las líneas
		// vacías de antes no pueden llevar más espacios que ella.
		maxVacia, conTexto := 0, false
		for i := ini; i < len(raws) && !conTexto; i++ {
			if vacia(raws[i]) {
				maxVacia = max(maxVacia, espacios(raws[i]))
			} else {
				n, conTexto = espacios(raws[i]), true
			}
		}
		switch {
		case !conTexto || n <= padre:
			// Bloque vacío: lo que sigue es del padre y las vacías son finales.
			n = max(padre+1, maxVacia)
		case maxVacia > n:
			return "", 0, errors.New("a leading empty line of the block scalar has more spaces than its first line")
		}
	}

	// Las líneas del bloque, ya sin la sangría; vacias[i] marca las vacías
	// (las que no tienen nada tras la sangría).
	var lineas []string
	var vacias []bool
	fin := ini
lineas:
	for ; fin < len(raws); fin++ {
		raw := raws[fin]
		sp := espacios(raw)
		switch {
		case vacia(raw) && sp <= n:
			lineas, vacias = append(lineas, ""), append(vacias, true)
		case sp >= n:
			lineas, vacias = append(lineas, raw[n:]), append(vacias, false)
		case strings.HasPrefix(raw[sp:], "#") || sp <= padre:
			// Un comentario menos sangrado (o lo siguiente del padre) acaba el bloque.
			break lineas
		default:
			return "", 0, fmt.Errorf("line %d is less indented than the block scalar above it", fin+1)
		}
	}
	// Las vacías del final son las de chomping, no del cuerpo.
	ultima := -1
	for i, v := range vacias {
		if !v {
			ultima = i
		}
	}
	var b strings.Builder
	if !plegado {
		b.WriteString(strings.Join(lineas[:ultima+1], "\n"))
	} else {
		// Plegado: el salto entre dos líneas normales es un espacio, o se
		// pierde si las separan vacías (cada vacía da un \n); junto a una línea
		// más sangrada (empieza por blanco) los saltos se conservan todos.
		normal := func(s string) bool { return s[0] != ' ' && s[0] != '\t' }
		prev, k := -1, 0
		for i := 0; i <= ultima; i++ {
			if vacias[i] {
				k++
				continue
			}
			switch {
			case prev < 0:
				b.WriteString(strings.Repeat("\n", k))
			case normal(lineas[prev]) && normal(lineas[i]) && k == 0:
				b.WriteByte(' ')
			case normal(lineas[prev]) && normal(lineas[i]):
				b.WriteString(strings.Repeat("\n", k))
			default:
				b.WriteString(strings.Repeat("\n", k+1))
			}
			b.WriteString(lineas[i])
			prev, k = i, 0
		}
	}
	// Chomping: strip quita el último salto y las vacías finales; clip deja
	// el salto; keep deja el salto y una \n por vacía final.
	conSalto := ultima >= 0 && (ini+ultima < len(raws)-1 || saltoFinal)
	switch chomp {
	case '-':
	case '+':
		if conSalto {
			b.WriteByte('\n')
		}
		for i := ultima + 1; i < len(lineas); i++ {
			if ini+i < len(raws)-1 || saltoFinal {
				b.WriteByte('\n')
			}
		}
	default:
		if conSalto {
			b.WriteByte('\n')
		}
	}
	return b.String(), fin, nil
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
			v, err := l.valorEnLinea(resto, ln.num)
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
			v, err := l.valorEnLinea(resto, ln.num)
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
func (l *lectorYAML) valorEnLinea(t string, num int) (any, error) {
	switch t[0] {
	case '|', '>':
		if v, ok := l.bloques[num]; ok {
			return v, nil
		}
		return nil, fmt.Errorf("line %d: invalid block scalar header %q", num, t)
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
	// Un cero delante (PIN: 0123, un código postal) se queda como texto: como
	// número perdería el cero, y en un entorno lo que cuenta es el texto.
	if d := strings.TrimLeft(s, "+-"); len(d) > 1 && d[0] == '0' {
		return s
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	return s
}
