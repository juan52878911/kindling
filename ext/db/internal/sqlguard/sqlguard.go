// Package sqlguard es la primera barrera de `kling db ask`: decide en el host,
// sin hablar con Postgres, si una sentencia que escribió un modelo tiene pinta
// de ser UNA consulta de lectura.
//
// NO es la garantía. La garantía es que la consulta se ejecuta con un rol de
// solo lectura, dentro de BEGIN TRANSACTION READ ONLY y con statement_timeout
// (ver cmd/kling-db/cmd_ask.go). Este filtro existe para rechazar pronto y con
// un mensaje claro lo que no es una consulta, y para cerrar lo que el rol no
// cierra: funciones que leen ficheros del servidor, esperas, bloqueos...
//
// Es deliberadamente estrecho: ante la duda, rechaza. Una pregunta de alguien
// que no programa no necesita dollar quoting, barras invertidas, varias
// sentencias ni escapes Unicode.
package sqlguard

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxLen acota la sentencia: una consulta de verdad cabe de sobra.
const MaxLen = 20000

// Error es un rechazo, con el motivo.
type Error struct{ Reason string }

func (e *Error) Error() string { return "rejected SQL: " + e.Reason }

func reject(format string, a ...any) error { return &Error{Reason: fmt.Sprintf(format, a...)} }

// Palabras clave que no tienen sitio en una consulta de lectura. Se comparan
// con las palabras SIN comillas (una columna "update" entre comillas es un
// nombre y no se toca).
var forbiddenWords = map[string]bool{
	// escrituras, también dentro de un WITH
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "TRUNCATE": true,
	// SELECT ... INTO crea una tabla
	"INTO": true,
	// esquema y permisos
	"DROP": true, "ALTER": true, "CREATE": true, "GRANT": true, "REVOKE": true,
	// órdenes que no son consultas: sin ';' no deberían poder aparecer, pero se
	// rechazan por nombre para que el motivo sea claro
	"COPY": true, "DO": true, "CALL": true, "SET": true, "RESET": true,
	"BEGIN": true, "COMMIT": true, "ROLLBACK": true, "ABORT": true, "SAVEPOINT": true,
	"PREPARE": true, "EXECUTE": true, "DEALLOCATE": true, "DISCARD": true,
	"LISTEN": true, "NOTIFY": true, "UNLISTEN": true, "LOCK": true, "VACUUM": true,
	"ANALYZE": true, "ANALYSE": true, "CLUSTER": true, "REINDEX": true, "REFRESH": true,
	"SECURITY": true, "IMPORT": true, "CHECKPOINT": true, "REASSIGN": true, "EXPLAIN": true,
	// FOR SHARE bloquea filas; UESCAPE deletrea nombres con escapes
	"SHARE": true, "NOWAIT": true, "UESCAPE": true,
}

// Funciones que se rechazan aunque vayan entre comillas o cualificadas
// (pg_catalog.pg_sleep): leen el sistema de ficheros, esperan, bloquean,
// señalan a otros procesos, cambian la configuración o ejecutan SQL guardado
// en una cadena (query_to_xml), que este filtro no podría inspeccionar.
var forbiddenFuncs = map[string]bool{
	"pg_read_file": true, "pg_read_binary_file": true, "pg_stat_file": true,
	"pg_logdir_ls": true, "set_config": true, "pg_terminate_backend": true,
	"pg_cancel_backend": true, "pg_reload_conf": true, "pg_rotate_logfile": true,
	"pg_promote": true, "pg_switch_wal": true, "pg_notify": true, "nextval": true,
	"setval": true, "query_to_xml": true, "query_to_xml_and_xmlschema": true,
	"query_to_xmlschema": true, "cursor_to_xml": true, "cursor_to_xmlschema": true,
	"pg_import_system_collations": true, "pg_log_backend_memory_contexts": true,
	"txid_current": true, "pg_current_xact_id": true,
}

// forbiddenPrefixes: familias enteras.
var forbiddenPrefixes = []string{
	"pg_sleep", "dblink", "lo_", "pg_ls_", "pg_file_", "pg_advisory_", "pg_try_advisory_",
	"pg_create_", "pg_drop_", "pg_replication_", "pg_logical_", "pg_stat_reset",
	"pg_backup_", "pg_start_backup", "pg_stop_backup", "pg_wal_replay_",
}

// forbiddenName dice si un nombre (en minúsculas) es de una función prohibida.
func forbiddenName(name string) bool {
	if forbiddenFuncs[name] {
		return true
	}
	for _, p := range forbiddenPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

type kind int

const (
	word   kind = iota // palabra sin comillas
	quoted             // identificador "entre comillas"
	str                // literal '...'
	symbol             // cualquier otro carácter suelto
)

type token struct {
	k    kind
	text string // word: en mayúsculas; quoted: tal cual (sin comillas); symbol: el carácter
	// glued: no hay espacio ni comentario entre este token y el anterior.
	glued bool
}

// Validate acepta una sola sentencia que empieza por SELECT o WITH y no toca
// nada de las listas de arriba. Devuelve *Error con el motivo si no.
func Validate(sql string) error {
	if strings.TrimSpace(sql) == "" {
		return reject("empty statement")
	}
	if len(sql) > MaxLen {
		return reject("longer than %d bytes", MaxLen)
	}
	if !utf8.ValidString(sql) {
		return reject("not valid UTF-8")
	}
	for _, r := range sql {
		switch {
		case r == '\\':
			// psql trata la barra invertida como metacomando (\! ejecuta una
			// orden) y E'...' la usa para escapes: ninguna de las dos hace
			// falta para preguntar.
			return reject("backslashes are not allowed")
		case r == '\t' || r == '\n' || r == '\r':
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return reject("control characters are not allowed")
		}
	}
	toks, err := tokenize(sql)
	if err != nil {
		return err
	}
	if len(toks) == 0 {
		return reject("only comments")
	}
	if toks[0].k != word || (toks[0].text != "SELECT" && toks[0].text != "WITH") {
		return reject("it must start with SELECT or WITH")
	}
	depth := 0
	for i, t := range toks {
		switch t.k {
		case symbol:
			switch t.text {
			case ";":
				return reject("';' is not allowed: one statement only")
			case "$":
				return reject("'$' is not allowed (dollar quoting or parameters)")
			case "(":
				depth++
			case ")":
				depth--
				if depth < 0 {
					return reject("unbalanced parentheses")
				}
			case "&":
				// U&'...' y U&"..." permiten deletrear nombres con escapes.
				if i > 0 && t.glued && toks[i-1].k == word && toks[i-1].text == "U" {
					return reject("Unicode escapes (U&) are not allowed")
				}
			}
		case word:
			if forbiddenWords[t.text] {
				return reject("%s is not allowed in a read-only query", t.text)
			}
			if forbiddenName(strings.ToLower(t.text)) {
				return reject("function %s is not allowed", strings.ToLower(t.text))
			}
			if t.text == "FOR" && i+1 < len(toks) && toks[i+1].k == word {
				// FOR UPDATE / FOR NO KEY UPDATE / FOR SHARE / FOR KEY SHARE bloquean filas.
				switch toks[i+1].text {
				case "NO", "KEY":
					return reject("row locks (FOR %s ...) are not allowed", toks[i+1].text)
				}
			}
			if t.text == "START" && i+1 < len(toks) && toks[i+1].k == word && toks[i+1].text == "TRANSACTION" {
				return reject("START TRANSACTION is not allowed")
			}
		case quoted:
			if forbiddenName(strings.ToLower(t.text)) {
				return reject("function %q is not allowed", t.text)
			}
		}
	}
	if depth != 0 {
		return reject("unbalanced parentheses")
	}
	return nil
}

// tokenize parte la sentencia en palabras, identificadores entre comillas,
// literales y símbolos, quitando los comentarios (-- y /* */ anidados, como
// Postgres). Lo que no cierra es un rechazo: una cadena o un comentario sin
// cerrar podría tragarse lo que va detrás al envolver la consulta.
func tokenize(s string) ([]token, error) {
	var toks []token
	glued := false
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
			glued = false
			continue
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				i = len(s)
			} else {
				i += j + 1
			}
			glued = false
			continue
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			depth := 0
			j := i
			for j < len(s) {
				if j+1 < len(s) && s[j] == '/' && s[j+1] == '*' {
					depth++
					j += 2
				} else if j+1 < len(s) && s[j] == '*' && s[j+1] == '/' {
					depth--
					j += 2
					if depth == 0 {
						break
					}
				} else {
					j++
				}
			}
			if depth != 0 {
				return nil, reject("unterminated comment")
			}
			i = j
			glued = false
			continue
		case c == '\'':
			j, err := closeQuote(s, i, '\'')
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{k: str, glued: glued})
			i = j
		case c == '"':
			j, err := closeQuote(s, i, '"')
			if err != nil {
				return nil, err
			}
			name := strings.ReplaceAll(s[i+1:j-1], `""`, `"`)
			toks = append(toks, token{k: quoted, text: name, glued: glued})
			i = j
		case isIdentStart(c):
			j := i
			for j < len(s) && isIdentChar(s[j]) {
				j++
			}
			toks = append(toks, token{k: word, text: strings.ToUpper(s[i:j]), glued: glued})
			i = j
		case c >= '0' && c <= '9':
			j := i
			for j < len(s) && (isIdentChar(s[j]) || s[j] == '.') {
				j++
			}
			toks = append(toks, token{k: symbol, text: "0", glued: glued})
			i = j
		default:
			toks = append(toks, token{k: symbol, text: string(c), glued: glued})
			i++
		}
		glued = true
	}
	return toks, nil
}

// closeQuote devuelve el índice justo después de la comilla que cierra la que
// abre en s[i], con la comilla doblada como escape.
func closeQuote(s string, i int, q byte) (int, error) {
	for j := i + 1; j < len(s); j++ {
		if s[j] != q {
			continue
		}
		if j+1 < len(s) && s[j+1] == q {
			j++
			continue
		}
		return j + 1, nil
	}
	if q == '"' {
		return 0, reject("unterminated quoted identifier")
	}
	return 0, reject("unterminated string")
}

// Los identificadores de Postgres admiten letras no ASCII: todo byte alto
// cuenta como letra. '$' no: se rechaza aparte.
func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentChar(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

// ErrNoSQL es una respuesta del modelo sin sentencia.
var ErrNoSQL = errors.New("the model did not return a SQL statement")

// Extract saca la sentencia de la respuesta de un modelo: el primer bloque
// ```sql ... ``` si lo hay, o el texto entero. Quita UN punto y coma final
// (los modelos lo ponen por costumbre); cualquier otro lo rechaza Validate.
func Extract(answer string) (string, error) {
	s := answer
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		// La etiqueta del bloque (sql, postgresql...) hasta el salto de línea.
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 && !strings.ContainsAny(rest[:nl], " ()") {
			rest = rest[nl+1:]
		}
		if j := strings.Index(rest, "```"); j >= 0 {
			rest = rest[:j]
		}
		s = rest
	}
	s = strings.TrimSpace(s)
	s = strings.TrimSpace(strings.TrimSuffix(s, ";"))
	if s == "" {
		return "", ErrNoSQL
	}
	return s, nil
}
