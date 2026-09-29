package dbmask

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// NewSalt es la sal de una construcción: 256 bits al azar, en hex (nada que
// escapar dentro de un literal SQL). Quien llama la usa para un MaskSQL y la
// descarta: no se guarda en ningún sitio, ni en el golden ni en el host.
func NewSalt() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

var saltPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// MaskMarker va en MaskSQL (los tests lo buscan).
const MaskMarker = "kling-db:clone-mask"

// quoteIdent cita un identificador de Postgres ("" dobla las comillas).
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// quoteLiteral cita un literal (con standard_conforming_strings, que MaskSQL
// fija: la barra no escapa nada).
func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// hashExpr es el hash del valor viejo v: sha256(sal || v::text) en hex.
//
// Solo depende de la sal y del valor, NO de la tabla ni de la columna: el mismo
// correo en users.email y en orders.customer_email da el mismo resultado, y
// los joins y las claves foráneas entre columnas enmascaradas con el mismo
// tipo siguen casando. La sal es secreta y se tira al acabar: sin ella no se
// puede comprobar un valor adivinado contra el enmascarado.
func hashExpr(salt, v string) string {
	return fmt.Sprintf("encode(sha256(convert_to('%s' || %s::text, 'UTF8')), 'hex')", salt, v)
}

// digits pasa el hash hex a cifras (a-f → 0-5): deterministas y del mismo largo.
func digits(h string) string { return "translate(" + h + ", 'abcdef', '012345')" }

// newValue es el valor nuevo de una columna, a partir del viejo v, sin el
// CAST ni el tratamiento de NULL.
func newValue(u Update, salt, v string) string {
	h := hashExpr(salt, v)
	switch u.Kind {
	case Email:
		return "'user_' || left(" + h + ", 16) || '@example.invalid'"
	case Name:
		return "'Person ' || upper(left(" + h + ", 8))"
	case Phone:
		if u.Category == "N" {
			return "'1555' || left(" + digits(h) + ", 7)"
		}
		return "'+1555' || left(" + digits(h) + ", 7)"
	case Card:
		return "'9999' || left(" + digits(h) + ", 12)"
	case Text:
		return "'text_' || left(" + h + ", 16)"
	case Fixed:
		return quoteLiteral(u.Value)
	}
	return "NULL"
}

// setExpr es la asignación de una columna. Los NULL siguen siendo NULL (salvo
// la regla null, que lo deja todo en NULL); el resto pasa por CAST al tipo de
// la columna: si no cabe (un correo en un entero), el UPDATE falla y la
// construcción entera se deshace.
func setExpr(u Update, salt, old string) string {
	if u.Kind == Null {
		return "NULL"
	}
	return fmt.Sprintf("CASE WHEN %s IS NULL THEN NULL ELSE CAST(%s AS %s) END", old, newValue(u, salt, old), u.Type)
}

// MaskSQL es el SQL que enmascara, para el psql del Postgres de construcción
// (superusuario, por el socket local, con ON_ERROR_STOP).
//
// Todo va en UNA transacción:
//
//   - session_replication_role = replica apaga los disparadores (los de las
//     claves foráneas y los del usuario: un disparador de auditoría no copia
//     los valores viejos a otra tabla mientras se enmascara);
//   - un UPDATE por tabla, unido a una instantánea de sí misma por
//     (tableoid, ctid), que devuelve por fila si cada columna tenía valor y si
//     el valor nuevo es IGUAL al viejo;
//   - los recuentos se imprimen (tabla|filas|con valor por columna|iguales por
//     columna) y, si alguna columna enmascarada conserva algún valor viejo,
//     1/0 aborta la transacción: no se confirma nada.
//
// Sin valores en la salida: solo recuentos. psql corre con VERBOSITY=sqlstate
// para que un error diga su código y no el valor que lo provocó.
func MaskSQL(p *Plan, salt string) (string, error) {
	if !saltPattern.MatchString(salt) {
		return "", errors.New("mask: bad salt")
	}
	var b strings.Builder
	b.WriteString("-- " + MaskMarker + "\n")
	b.WriteString("BEGIN;\n")
	b.WriteString("SET LOCAL standard_conforming_strings = on;\n")
	b.WriteString("SET LOCAL session_replication_role = replica;\n")
	b.WriteString("SET LOCAL statement_timeout = 0;\n")
	b.WriteString("CREATE TEMP TABLE kling_clone_result (i int PRIMARY KEY, n bigint NOT NULL, masked bigint[] NOT NULL, same bigint[] NOT NULL) ON COMMIT DROP;\n")
	for i, t := range p.Tables {
		rel := quoteIdent(t.Schema) + "." + quoteIdent(t.Table)
		only := "ONLY "
		if t.Partitioned {
			only = "" // una tabla particionada no admite ONLY: el UPDATE baja a sus particiones
		}
		var cols, sets, rets, masked, same []string
		for j, u := range t.Updates {
			old := fmt.Sprintf("o.kc_%d", j)
			cols = append(cols, fmt.Sprintf("%s AS kc_%d", quoteIdent(u.Column), j))
			sets = append(sets, fmt.Sprintf("%s = %s", quoteIdent(u.Column), setExpr(u, salt, old)))
			// "same": tenía valor y el nuevo es idéntico (como texto: json y
			// otros tipos no tienen igualdad). Con fixed no cuenta: un valor
			// que ya era el fijo es legítimo.
			sameExpr := "false"
			if u.Kind != Fixed {
				sameExpr = fmt.Sprintf("(%s IS NOT NULL AND t.%s::text IS NOT DISTINCT FROM %s::text)", old, quoteIdent(u.Column), old)
			}
			rets = append(rets, fmt.Sprintf("(%s IS NOT NULL) AS nn_%d, %s AS eq_%d", old, j, sameExpr, j))
			masked = append(masked, fmt.Sprintf("count(*) FILTER (WHERE nn_%d)", j))
			same = append(same, fmt.Sprintf("count(*) FILTER (WHERE eq_%d)", j))
		}
		fmt.Fprintf(&b, "WITH o AS (SELECT tableoid AS kc_tab, ctid AS kc_tid, %s FROM %s%s),\n", strings.Join(cols, ", "), only, rel)
		fmt.Fprintf(&b, "u AS (UPDATE %s%s AS t SET %s FROM o WHERE t.tableoid = o.kc_tab AND t.ctid = o.kc_tid RETURNING %s)\n",
			only, rel, strings.Join(sets, ", "), strings.Join(rets, ", "))
		fmt.Fprintf(&b, "INSERT INTO kling_clone_result SELECT %d, count(*), ARRAY[%s]::bigint[], ARRAY[%s]::bigint[] FROM u;\n",
			i, strings.Join(masked, ", "), strings.Join(same, ", "))
	}
	b.WriteString("SELECT 'kc', i, n, array_to_string(masked, ','), array_to_string(same, ',') FROM kling_clone_result ORDER BY i;\n")
	b.WriteString("SELECT 1 / (CASE WHEN EXISTS (SELECT 1 FROM kling_clone_result, unnest(same) AS s WHERE s > 0) THEN 0 ELSE 1 END);\n")
	b.WriteString("COMMIT;\n")
	return b.String(), nil
}

// ColumnResult es lo que el enmascarado dice de una columna.
type ColumnResult struct {
	Masked int64 // filas con valor que cambiaron
	Same   int64 // filas cuyo valor no cambió (tiene que ser 0)
}

// Result es, por tabla del plan (mismo orden), filas y recuentos por columna.
type Result struct {
	Rows    []int64
	Columns [][]ColumnResult
}

// ParseResult lee lo que imprime MaskSQL (psql -At, separador |). Solo mira
// las líneas que empiezan por "kc|".
func ParseResult(p *Plan, out []byte) (*Result, error) {
	r := &Result{Rows: make([]int64, len(p.Tables)), Columns: make([][]ColumnResult, len(p.Tables))}
	seen := make([]bool, len(p.Tables))
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) != 5 || f[0] != "kc" {
			continue
		}
		i, err := strconv.Atoi(f[1])
		if err != nil || i < 0 || i >= len(p.Tables) || seen[i] {
			return nil, errors.New("mask: unexpected result line")
		}
		n, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil {
			return nil, errors.New("mask: unexpected result line")
		}
		m, err1 := ints(f[3])
		s, err2 := ints(f[4])
		if err1 != nil || err2 != nil || len(m) != len(p.Tables[i].Updates) || len(s) != len(m) {
			return nil, errors.New("mask: unexpected result line")
		}
		seen[i] = true
		r.Rows[i] = n
		for j := range m {
			r.Columns[i] = append(r.Columns[i], ColumnResult{Masked: m[j], Same: s[j]})
		}
	}
	for i, ok := range seen {
		if !ok {
			return nil, fmt.Errorf("mask: no result for %s.%s", p.Tables[i].Schema, p.Tables[i].Table)
		}
	}
	return r, nil
}

func ints(s string) ([]int64, error) {
	if s == "" {
		return nil, nil
	}
	var out []int64
	for _, p := range strings.Split(s, ",") {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// Unchanged devuelve las columnas que conservan algún valor viejo (solo sus
// nombres). Vacío es lo único aceptable.
func (r *Result) Unchanged(p *Plan) []string {
	var out []string
	for i, t := range p.Tables {
		for j, u := range t.Updates {
			if j < len(r.Columns[i]) && r.Columns[i][j].Same > 0 {
				out = append(out, t.Schema+"."+t.Table+"."+u.Column)
			}
		}
	}
	return out
}
