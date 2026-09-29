package doctor

import (
	"regexp"
	"strings"
)

// Detección de políticas RLS "fail-open": expresiones que dejan pasar TODAS
// las filas cuando la variable de tenant (current_setting) no está puesta o
// está vacía. Es la trampa de AuraCRM:
//
//	USING (current_setting('app.tenant_id', true) IS NULL
//	       OR current_setting('app.tenant_id', true) = ''
//	       OR tenant_id = current_setting('app.tenant_id', true)::uuid)
//
// Con missing_ok = true, una conexión que olvida el SET ve todos los tenants.
// No es un analizador de SQL: reconoce las formas que escribe la gente y las
// que devuelve pg_policies (deparseadas, con paréntesis y casts ::text), y
// prefiere avisar de más a callarse.

var reTenantVar = regexp.MustCompile(`(?i)current_setting\(\s*'([^']{1,63})'`)

// tenantVars devuelve las variables que la expresión lee con current_setting.
func tenantVars(expr string) []string {
	var out []string
	for _, m := range reTenantVar.FindAllStringSubmatch(expr, -1) {
		out = append(out, m[1])
	}
	return out
}

// failOpen dice si expr deja pasar todo con la variable nula o vacía, y por qué.
func failOpen(expr string) (string, bool) {
	low := maskLiterals(asciiLower(expr))
	for i := 0; ; {
		j := indexWord(low[i:], "current_setting(")
		if j < 0 {
			break
		}
		start := i + j
		end := matchParen(low, start+len("current_setting"))
		if end < 0 {
			break
		}
		if r, ok := nullOrEmptyTest(low, start, end+1); ok {
			return r, true
		}
		i = end + 1
	}
	for i := 0; ; {
		j := indexWord(low[i:], "coalesce(")
		if j < 0 {
			break
		}
		open := i + j + len("coalesce")
		end := matchParen(low, open)
		if end < 0 {
			break
		}
		args := splitArgs(low[open+1 : end])
		if len(args) > 1 && strings.Contains(args[0], "current_setting(") {
			for _, a := range args[1:] {
				switch v := bare(a); {
				case v == "true":
					return "COALESCE(<tenant check>, true) is true when the setting is missing", true
				case reColumn.MatchString(v) && !sqlWords[v]:
					return "COALESCE(<tenant setting>, " + v + ") compares the column with itself when the setting is missing", true
				}
			}
		}
		i = open + 1
	}
	return "", false
}

// nullOrEmptyTest mira si la llamada low[s:e] (current_setting(...)), tras
// subir por los envoltorios (NULLIF, btrim, casts, paréntesis), se compara
// con NULL o con la cadena vacía.
func nullOrEmptyTest(low string, s, e int) (string, bool) {
	for {
		e = skipCasts(low, e)
		p := skipSpaceBack(low, s)
		if p == 0 || low[p-1] != '(' {
			break
		}
		// ¿Es la llamada a una función que envuelve el valor, o un paréntesis
		// de agrupación que cierra justo después?
		q := p - 1
		k := q
		for k > 0 && isIdent(low[k-1]) {
			k--
		}
		if fn := low[k:q]; fn != "" {
			if fn == "coalesce" || fn == "when" {
				break
			}
			cl := matchParen(low, q)
			if cl < 0 {
				break
			}
			s, e = k, cl+1
			continue
		}
		a := skipSpace(low, e)
		if a < len(low) && low[a] == ')' {
			s, e = q, a+1
			continue
		}
		break
	}
	// Una rama CASE WHEN x IS NULL THEN false ... cierra, no abre.
	if b := strings.TrimRight(low[:s], " ("); strings.HasSuffix(b, "when") {
		return "", false
	}
	rest := low[skipSpace(low, e):]
	switch {
	case strings.HasPrefix(rest, "is null"):
		return "<tenant setting> IS NULL is true when the setting is missing", true
	case reEqEmpty.MatchString(rest):
		return "<tenant setting> = '' is true when the setting is empty", true
	}
	if reEmptyEq.MatchString(low[:s]) {
		return "'' = <tenant setting> is true when the setting is empty", true
	}
	return "", false
}

var (
	reEqEmpty = regexp.MustCompile(`^=\s*''(\s*::\s*[a-z ]+)?`)
	reEmptyEq = regexp.MustCompile(`''(\s*::\s*[a-z]+)?\s*=\s*\(*\s*$`)
	reColumn  = regexp.MustCompile(`^([a-z_][a-z0-9_$]*\.)?[a-z_][a-z0-9_$]*$`)
	sqlWords  = map[string]bool{"null": true, "false": true, "current_user": true, "session_user": true, "current_role": true, "user": true}
)

// bare quita espacios, paréntesis exteriores y casts de un argumento.
func bare(a string) string {
	a = strings.TrimSpace(a)
	for {
		if i := strings.LastIndex(a, "::"); i > 0 && reColumn.MatchString(strings.TrimSpace(a[i+2:])) {
			a = strings.TrimSpace(a[:i])
			continue
		}
		if len(a) > 1 && a[0] == '(' && matchParen(a, 0) == len(a)-1 {
			a = strings.TrimSpace(a[1 : len(a)-1])
			continue
		}
		return a
	}
}

// skipCasts salta los "::tipo" que siguen a low[:e] ("::text",
// "::character varying", "::uuid[]").
func skipCasts(low string, e int) int {
	for {
		i := skipSpace(low, e)
		if !strings.HasPrefix(low[i:], "::") {
			return e
		}
		j := skipSpace(low, i+2)
		k := j
		for k < len(low) && isIdent(low[k]) {
			k++
		}
		if k == j {
			return e
		}
		switch w := low[j:k]; {
		case w == "character" && strings.HasPrefix(low[k:], " varying"):
			k += len(" varying")
		case w == "double" && strings.HasPrefix(low[k:], " precision"):
			k += len(" precision")
		}
		if strings.HasPrefix(low[k:], "[]") {
			k += 2
		}
		e = k
	}
}

func skipSpace(s string, i int) int {
	for i < len(s) && s[i] == ' ' {
		i++
	}
	return i
}

func skipSpaceBack(s string, i int) int {
	for i > 0 && s[i-1] == ' ' {
		i--
	}
	return i
}

func isIdent(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}

// indexWord busca w (en minúsculas) como palabra: sin letra delante.
func indexWord(s, w string) int {
	for off := 0; ; {
		i := strings.Index(s[off:], w)
		if i < 0 {
			return -1
		}
		i += off
		if i == 0 || !isIdent(s[i-1]) {
			return i
		}
		off = i + 1
	}
}

// matchParen devuelve el índice del ')' que cierra el '(' de s[open], saltando
// literales '...' e identificadores "...". -1 si no cierra.
func matchParen(s string, open int) int {
	if open >= len(s) || s[open] != '(' {
		return -1
	}
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '\'', '"':
			q := s[i]
			for i++; i < len(s); i++ {
				if s[i] == q {
					if i+1 < len(s) && s[i+1] == q {
						i++
						continue
					}
					break
				}
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// splitArgs parte una lista de argumentos por las comas de primer nivel.
func splitArgs(s string) []string {
	var out []string
	depth, last := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'', '"':
			q := s[i]
			for i++; i < len(s) && s[i] != q; i++ {
			}
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[last:i])
				last = i + 1
			}
		}
	}
	return append(out, s[last:])
}

// maskLiterals cambia por espacios el contenido de los literales '...' y de
// los identificadores "...", conservando las comillas y la longitud: lo que
// va dentro de una cadena no es código (y la cadena vacía sigue siendo dos comillas).
func maskLiterals(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		if b[i] != '\'' && b[i] != '"' {
			continue
		}
		q := b[i]
		for i++; i < len(b); i++ {
			if b[i] == q {
				if i+1 < len(b) && b[i+1] == q {
					b[i], b[i+1] = ' ', ' '
					i++
					continue
				}
				break
			}
			b[i] = ' '
		}
	}
	return string(b)
}

// asciiLower pasa a minúsculas solo A-Z: los índices siguen valiendo para el
// texto original (strings.ToLower puede cambiar la longitud en UTF-8), y los
// saltos de línea y tabuladores pasan a espacio.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 'A' && c <= 'Z':
			b[i] = c + 32
		case c == '\n' || c == '\t' || c == '\r':
			b[i] = ' '
		}
	}
	return string(b)
}
