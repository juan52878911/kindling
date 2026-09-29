// Package dbmask es la parte pura de `kling db clone`: las reglas de
// enmascarado, la detección de columnas sospechosas, el plan, el SQL que lo
// aplica dentro de la microVM de construcción y el informe final.
//
// Nada de aquí habla con kindling ni con Postgres: recibe el catálogo (JSON que
// devuelve CatalogSQL) y devuelve SQL y texto. Por eso se prueba entero sin
// base de datos, y por eso es fácil ver que el informe no lleva valores: el
// catálogo no los tiene y el resultado del enmascarado solo trae recuentos.
package dbmask

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Kind es el tipo de una regla.
type Kind string

const (
	Email Kind = "email" // user_<hash>@example.invalid
	Name  Kind = "name"  // Person <HASH>
	Phone Kind = "phone" // +1555 y 7 cifras (sin '+' en columnas numéricas)
	Card  Kind = "card"  // 9999 y 12 cifras (prefijo que no emite ninguna red)
	Text  Kind = "text"  // text_<hash>
	Null  Kind = "null"  // NULL
	Keep  Kind = "keep"  // se deja tal cual, a propósito
	Fixed Kind = "fixed" // fixed:<valor>, el mismo valor en todas las filas
)

// hashed son los tipos que derivan el valor nuevo de un hash del viejo: los
// que conservan relaciones y joins.
var hashed = map[Kind]bool{Email: true, Name: true, Phone: true, Card: true, Text: true}

// Rule es una regla: una columna y qué se hace con ella.
type Rule struct {
	Schema, Table, Column string
	Kind                  Kind
	// Value es el valor de fixed. No sale nunca en el informe.
	Value string
}

// Key es schema.table.column tal cual (sin comillas).
func (r Rule) Key() string { return r.Schema + "." + r.Table + "." + r.Column }

// Rules son las reglas de un fichero, por columna.
type Rules struct {
	list []Rule
	by   map[string]Rule
}

// Len es cuántas reglas hay.
func (rs *Rules) Len() int { return len(rs.list) }

// Get devuelve la regla de una columna.
func (rs *Rules) Get(schema, table, column string) (Rule, bool) {
	r, ok := rs.by[schema+"."+table+"."+column]
	return r, ok
}

// List devuelve las reglas en orden de columna.
func (rs *Rules) List() []Rule { return append([]Rule(nil), rs.list...) }

// Límites del fichero de reglas: es de una persona, no de un volcado.
const (
	maxRulesBytes = 1 << 20
	maxRules      = 10000
	maxIdent      = 63 // NAMEDATALEN-1 de Postgres
	maxFixed      = 1024
)

// ParseRules lee un fichero de reglas. Dos formatos, los dos planos:
//
// JSON, un objeto de "tabla.columna" (o "esquema.tabla.columna") a tipo:
//
//	{"users.email": "email", "billing.cards.number": "card", "users.bio": "fixed:redacted"}
//
// o el mismo mapa en YAML simple, una regla por línea (# comenta):
//
//	users.email: email
//	users.bio: "fixed:redacted"
//
// Sin esquema, public. Una columna repetida, un tipo desconocido o un nombre
// con caracteres de control son errores: un fichero de reglas ambiguo no se
// interpreta, se rechaza.
func ParseRules(data []byte) (*Rules, error) {
	if len(data) > maxRulesBytes {
		return nil, fmt.Errorf("rules file over %d bytes", maxRulesBytes)
	}
	var pairs [][2]string
	var err error
	if t := bytes.TrimSpace(data); len(t) > 0 && t[0] == '{' {
		pairs, err = parseJSON(t)
	} else {
		pairs, err = parseYAML(data)
	}
	if err != nil {
		return nil, err
	}
	rs := &Rules{by: map[string]Rule{}}
	for _, p := range pairs {
		r, err := parseRule(p[0], p[1])
		if err != nil {
			return nil, err
		}
		if _, dup := rs.by[r.Key()]; dup {
			return nil, fmt.Errorf("rule for %s given twice", r.Key())
		}
		rs.by[r.Key()] = r
		rs.list = append(rs.list, r)
		if len(rs.list) > maxRules {
			return nil, fmt.Errorf("more than %d rules", maxRules)
		}
	}
	sort.Slice(rs.list, func(i, j int) bool { return rs.list[i].Key() < rs.list[j].Key() })
	return rs, nil
}

// parseJSON lee el objeto token a token para ver las claves repetidas, que
// encoding/json se traga (la última gana) sin avisar.
func parseJSON(data []byte) ([][2]string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("rules: JSON must be an object of \"table.column\": \"kind\"")
	}
	var out [][2]string
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("rules: bad JSON: %v", err)
		}
		k, _ := kt.(string)
		vt, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("rules: bad JSON: %v", err)
		}
		v, ok := vt.(string)
		if !ok {
			return nil, fmt.Errorf("rules: the kind of %q must be a string", k)
		}
		out = append(out, [2]string{k, v})
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, errors.New("rules: bad JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("rules: trailing data after the JSON object")
	}
	return out, nil
}

// parseYAML es el subconjunto de YAML de un mapa plano: "clave: valor", con
// comillas opcionales en el valor y comentarios con #. Nada de anidar, listas
// ni anclas: si hace falta más, es JSON.
func parseYAML(data []byte) ([][2]string, error) {
	var out [][2]string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), maxRulesBytes)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || line == "---" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("rules line %d: want \"table.column: kind\"", n)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		k = unquote(k)
		switch {
		case strings.HasPrefix(v, `"`) || strings.HasPrefix(v, `'`):
			q := v[:1]
			end := strings.LastIndex(v, q)
			if end == 0 {
				return nil, fmt.Errorf("rules line %d: unterminated quote", n)
			}
			if rest := strings.TrimSpace(v[end+1:]); rest != "" && !strings.HasPrefix(rest, "#") {
				return nil, fmt.Errorf("rules line %d: text after the quoted value", n)
			}
			v = v[1:end]
		default:
			if i := strings.Index(v, " #"); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
		}
		out = append(out, [2]string{k, v})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("rules: %v", err)
	}
	return out, nil
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

func parseRule(key, val string) (Rule, error) {
	parts := strings.Split(key, ".")
	var r Rule
	switch len(parts) {
	case 2:
		r.Schema, r.Table, r.Column = "public", parts[0], parts[1]
	case 3:
		r.Schema, r.Table, r.Column = parts[0], parts[1], parts[2]
	default:
		return r, fmt.Errorf("rule %q: want table.column or schema.table.column", safeText(key))
	}
	for _, id := range []string{r.Schema, r.Table, r.Column} {
		if err := validIdent(id); err != nil {
			return r, fmt.Errorf("rule %q: %v", safeText(key), err)
		}
	}
	if v, ok := strings.CutPrefix(val, "fixed:"); ok {
		if len(v) > maxFixed || !utf8.ValidString(v) || hasControl(v) {
			return r, fmt.Errorf("rule %s: fixed value must be printable UTF-8 of at most %d bytes", r.Key(), maxFixed)
		}
		r.Kind, r.Value = Fixed, v
		return r, nil
	}
	switch k := Kind(val); k {
	case Email, Name, Phone, Card, Text, Null, Keep:
		r.Kind = k
	default:
		return r, fmt.Errorf("rule %s: unknown kind %q (email, name, phone, card, text, null, keep, fixed:<value>)", r.Key(), safeText(val))
	}
	return r, nil
}

func validIdent(s string) error {
	switch {
	case s == "":
		return errors.New("empty name")
	case len(s) > maxIdent:
		return fmt.Errorf("name over %d bytes", maxIdent)
	case !utf8.ValidString(s) || hasControl(s):
		return errors.New("name with control characters")
	}
	return nil
}

func hasControl(s string) bool {
	for _, c := range s {
		if unicode.IsControl(c) {
			return true
		}
	}
	return false
}

// safeText acota lo que se cita de la entrada en un error.
func safeText(s string) string {
	var b strings.Builder
	for _, c := range s {
		if b.Len() >= 64 {
			b.WriteString("…")
			break
		}
		if unicode.IsControl(c) {
			c = '?'
		}
		b.WriteRune(c)
	}
	return b.String()
}
