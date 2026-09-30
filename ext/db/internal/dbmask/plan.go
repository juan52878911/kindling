package dbmask

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// ── catálogo ─────────────────────────────────────────────────────────────────

// Column es una columna de una tabla del catálogo. Sin valores: nombre, tipo y
// poco más.
type Column struct {
	Schema    string `json:"schema"`
	Table     string `json:"table"`
	Column    string `json:"column"`
	Type      string `json:"type"`      // format_type(): sintaxis SQL válida
	Category  string `json:"category"`  // pg_type.typcategory (S texto, N número, I red…)
	Generated bool   `json:"generated"` // columna generada (no se puede UPDATE)
}

// Table es una tabla con datos propios (relkind r o p).
type Table struct {
	Schema      string `json:"schema"`
	Name        string `json:"name"`
	Partitioned bool   `json:"partitioned"` // relkind p: el UPDATE va a sus particiones
	// Root es, en una partición, su tabla particionada raíz (esquema.tabla).
	// Sus columnas no se revisan aparte: el UPDATE de la raíz las cubre.
	Root string `json:"root,omitempty"`
	Rows int64  `json:"rows"`
}

// Catalog es lo que devuelve CatalogSQL.
type Catalog struct {
	Tables  []Table  `json:"tables"`
	Columns []Column `json:"columns"`
}

// CatalogMarker va en CatalogSQL (los tests lo buscan).
const CatalogMarker = "kling-db:clone-catalog"

// CatalogSQL devuelve, en UNA línea de JSON, las tablas con datos del usuario
// (con su número de filas) y sus columnas. Sin valores: solo nombres, tipos y
// recuentos. Se ejecuta en el Postgres de la construcción, nunca en producción.
const CatalogSQL = `-- ` + CatalogMarker + `
WITH t AS (
  SELECT c.oid, n.nspname, c.relname, c.relkind, c.relispartition
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
  WHERE c.relkind IN ('r', 'p')
    AND n.nspname NOT IN ('pg_catalog', 'information_schema')
    AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp%'
)
SELECT json_build_object(
  'tables', COALESCE((SELECT json_agg(json_build_object(
      'schema', t.nspname, 'name', t.relname, 'partitioned', t.relkind = 'p',
      'root', CASE WHEN t.relispartition THEN (SELECT rn.nspname || '.' || rc.relname
                 FROM pg_class rc JOIN pg_namespace rn ON rn.oid = rc.relnamespace
                 WHERE rc.oid = pg_partition_root(t.oid)) END,
      'rows', CASE WHEN t.relkind = 'r' THEN (xpath('/row/c/text()', query_to_xml(
                 format('SELECT count(*) AS c FROM ONLY %I.%I', t.nspname, t.relname), false, true, '')))[1]::text::bigint
               ELSE 0 END)
    ORDER BY t.nspname, t.relname) FROM t), '[]'::json),
  'columns', COALESCE((SELECT json_agg(json_build_object(
      'schema', t.nspname, 'table', t.relname, 'column', a.attname,
      'type', format_type(a.atttypid, a.atttypmod), 'category', ty.typcategory,
      'generated', a.attgenerated <> '')
    ORDER BY t.nspname, t.relname, a.attnum)
    FROM t JOIN pg_attribute a ON a.attrelid = t.oid JOIN pg_type ty ON ty.oid = a.atttypid
    WHERE a.attnum > 0 AND NOT a.attisdropped), '[]'::json));
`

// ParseCatalog lee la salida de CatalogSQL.
func ParseCatalog(out []byte) (*Catalog, error) {
	var c Catalog
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &c); err != nil {
		return nil, fmt.Errorf("reading the catalog: %v", err)
	}
	return &c, nil
}

// ── detección ────────────────────────────────────────────────────────────────

// suspiciousTokens son palabras que, como trozo del nombre de una columna
// (separado por _, -, cifras o mayúsculas), la hacen sospechosa.
var suspiciousTokens = map[string]string{
	"email": "email", "mail": "email", "e-mail": "email",
	"phone": "phone", "telephone": "phone", "tel": "phone", "mobile": "phone", "cell": "phone", "fax": "phone",
	"name": "name", "firstname": "name", "lastname": "name", "surname": "name", "fullname": "name", "username": "name",
	"dni": "dni", "nif": "dni", "nie": "dni", "ssn": "dni", "passport": "dni", "cif": "dni",
	"iban": "iban", "bic": "iban", "swift": "iban",
	"card": "card", "pan": "card", "cvv": "card", "cvc": "card", "ccn": "card",
	"address": "address", "addr": "address", "street": "address", "zip": "address", "postcode": "address", "postal": "address",
	"ip": "ip", "ipaddr": "ip", "ipaddress": "ip",
	"birth": "birth", "birthdate": "birth", "dob": "birth",
}

// suspiciousSubstrings se buscan dentro del nombre entero (emailaddress,
// userphone…): son lo bastante largas para no dar falsos positivos tontos.
var suspiciousSubstrings = []struct{ s, why string }{
	{"email", "email"}, {"phone", "phone"}, {"iban", "iban"}, {"address", "address"},
	{"passport", "dni"}, {"firstname", "name"}, {"lastname", "name"}, {"surname", "name"},
	{"fullname", "name"}, {"birthdate", "birth"},
}

// credentialTokens son palabras que, como trozo del nombre, delatan una
// credencial: contraseñas (y sus hashes, sales y cifrados), secretos, tokens,
// semillas de OTP y credenciales enteras. Un hash de contraseña también se
// marca: con él se ataca la contraseña por fuerza bruta fuera de línea.
var credentialTokens = map[string]bool{
	"password": true, "passwd": true, "pwd": true, "pass": true, "passphrase": true, "passcode": true,
	"secret": true, "secrets": true, "token": true, "jwt": true, "salt": true, "bearer": true,
	"credential": true, "credentials": true, "creds": true,
	"otp": true, "totp": true, "hotp": true, "mfa": true, "apikey": true, "privkey": true,
}

// credentialKeyPrefixes son las palabras que, delante de key, la convierten en
// una clave de verdad (api_key, private_key, secret_key…); key sola no basta
// (primary_key, sort_key, key en una tabla clave-valor).
var credentialKeyPrefixes = map[string]bool{
	"api": true, "private": true, "priv": true, "secret": true, "access": true, "signing": true,
	"encryption": true, "master": true, "client": true, "auth": true, "ssh": true, "gpg": true,
	"pgp": true, "hmac": true, "license": true, "live": true, "webhook": true,
}

// credentialSubstrings se buscan dentro del nombre entero, para los pegados
// (userpassword, accesstoken, apikeyhash…).
var credentialSubstrings = []string{
	"password", "passwd", "passphrase", "secret", "credential", "apikey", "privatekey",
	"accesstoken", "refreshtoken", "authtoken", "sessiontoken", "resettoken", "apitoken",
	"idtoken", "bearertoken", "csrftoken", "otpseed", "totpseed",
}

// Credential dice si el nombre de una columna parece una credencial.
func Credential(column string) bool {
	toks := tokens(column)
	for i, tok := range toks {
		if credentialTokens[tok] {
			return true
		}
		if tok == "key" && i > 0 && credentialKeyPrefixes[toks[i-1]] {
			return true
		}
	}
	// secretary/secretaría no son secretos.
	low := strings.ReplaceAll(strings.ToLower(column), "secretar", "")
	for _, s := range credentialSubstrings {
		if strings.Contains(low, s) {
			return true
		}
	}
	return false
}

// Suspicious dice si el nombre de una columna parece de un dato personal o de
// una credencial y por qué ("" si no). Es una heurística por nombre: se
// equivoca hacia el lado de bloquear (table_name es sospechosa; se marca keep
// y listo). Las credenciales van primero: password_hash dice "credential".
func Suspicious(column string) string {
	if Credential(column) {
		return "credential"
	}
	for _, tok := range tokens(column) {
		if why, ok := suspiciousTokens[tok]; ok {
			return why
		}
	}
	low := strings.ToLower(column)
	for _, s := range suspiciousSubstrings {
		if strings.Contains(low, s.s) {
			return s.why
		}
	}
	return ""
}

// tokens parte un nombre en palabras en minúscula: por _, -, espacios,
// cifras y el paso de minúscula a mayúscula (firstName → first, name).
func tokens(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	var prev rune
	for _, c := range s {
		switch {
		case !unicode.IsLetter(c):
			flush()
		case unicode.IsUpper(c) && unicode.IsLower(prev):
			flush()
			cur = append(cur, c)
		default:
			cur = append(cur, c)
		}
		prev = c
	}
	flush()
	return out
}

// strictTypes son los tipos que -strict exige tratar aunque el nombre no diga
// nada: texto libre, JSON, XML, binario y arrays.
func strictType(c Column) bool {
	switch c.Category {
	case "S", "A":
		return true
	}
	base := strings.ToLower(c.Type)
	for _, t := range []string{"json", "jsonb", "xml", "bytea"} {
		if base == t {
			return true
		}
	}
	return false
}

// ── plan ─────────────────────────────────────────────────────────────────────

// Options gobierna el plan.
type Options struct {
	// AllowUnmasked deja construir con columnas sospechosas sin regla: quedan
	// tal cual y el informe lo dice.
	AllowUnmasked bool
	// Strict hace sospechosa además toda columna de texto, JSON, XML, bytea o
	// array, se llame como se llame.
	Strict bool
}

// Action es qué se hizo con una columna sospechosa.
type Action string

const (
	ActMasked    Action = "masked"    // una regla la cambia
	ActKept      Action = "kept"      // regla keep: se dejó a propósito
	ActUnmasked  Action = "UNMASKED"  // sin regla, con -allow-unmasked
	ActGenerated Action = "generated" // columna generada: se recalcula de sus entradas
)

// Finding es una columna sospechosa y lo que se hizo.
type Finding struct {
	Schema, Table, Column string
	Reason                string // email, phone, name, type… (por qué es sospechosa)
	Action                Action
	Kind                  Kind // la regla, si la hay
}

// Key es schema.table.column.
func (f Finding) Key() string { return f.Schema + "." + f.Table + "." + f.Column }

// Update es una columna que el enmascarado cambia.
type Update struct {
	Column   string
	Type     string
	Category string
	Kind     Kind
	Value    string // fixed
}

// TableUpdate es un UPDATE: una tabla y sus columnas.
type TableUpdate struct {
	Schema, Table string
	Partitioned   bool
	Rows          int64
	Updates       []Update
}

// Plan es lo que se va a hacer, antes de hacerlo.
type Plan struct {
	Tables   []TableUpdate
	Kept     []Rule // reglas keep
	Findings []Finding
	// TableCount y RowCount son del catálogo entero (tablas con datos propios).
	TableCount int
	RowCount   int64
}

// ErrUnmasked es el error de un plan con columnas sospechosas sin regla.
var ErrUnmasked = errors.New("suspicious columns without a rule")

// UnmaskedError lista las columnas (solo nombres) que bloquean la construcción.
type UnmaskedError struct{ Columns []Finding }

func (e *UnmaskedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d suspicious column(s) without a rule; add a rule (keep if it is not personal data) or pass -allow-unmasked:", len(e.Columns))
	for _, f := range e.Columns {
		fmt.Fprintf(&b, "\n  %s  (%s)", f.Key(), f.Reason)
	}
	return b.String()
}

func (e *UnmaskedError) Unwrap() error { return ErrUnmasked }

// NewPlan cruza el catálogo con las reglas. Falla si una regla no casa con
// ninguna columna (una errata dejaría una columna sin tratar creyendo que lo
// está), si apunta a una columna generada o a una partición, o si queda una
// columna sospechosa sin regla y no se pasó AllowUnmasked.
func NewPlan(cat *Catalog, rules *Rules, o Options) (*Plan, error) {
	tables := map[string]*Table{}
	p := &Plan{}
	for i := range cat.Tables {
		t := &cat.Tables[i]
		tables[t.Schema+"."+t.Name] = t
		p.TableCount++
		p.RowCount += t.Rows
	}
	cols := map[string]Column{}
	for _, c := range cat.Columns {
		cols[c.Schema+"."+c.Table+"."+c.Column] = c
	}

	var errs []string
	byTable := map[string]*TableUpdate{}
	for _, r := range rules.List() {
		c, ok := cols[r.Key()]
		if !ok {
			errs = append(errs, fmt.Sprintf("rule for %s: no such column in the source", r.Key()))
			continue
		}
		t := tables[c.Schema+"."+c.Table]
		if t != nil && t.Root != "" {
			errs = append(errs, fmt.Sprintf("rule for %s: %s.%s is a partition; put the rule on %s", r.Key(), c.Schema, c.Table, t.Root))
			continue
		}
		if r.Kind == Keep {
			p.Kept = append(p.Kept, r)
			continue
		}
		if c.Generated {
			errs = append(errs, fmt.Sprintf("rule for %s: it is a generated column (mask its inputs instead, or use keep)", r.Key()))
			continue
		}
		tk := c.Schema + "." + c.Table
		tu := byTable[tk]
		if tu == nil {
			tu = &TableUpdate{Schema: c.Schema, Table: c.Table}
			if t != nil {
				tu.Partitioned, tu.Rows = t.Partitioned, t.Rows
			}
			byTable[tk] = tu
		}
		tu.Updates = append(tu.Updates, Update{Column: c.Column, Type: c.Type, Category: c.Category, Kind: r.Kind, Value: r.Value})
	}
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "\n"))
	}

	var unmasked []Finding
	for _, c := range cat.Columns {
		if t := tables[c.Schema+"."+c.Table]; t != nil && t.Root != "" {
			continue // la cubre la regla (o el hallazgo) de su tabla raíz
		}
		reason := Suspicious(c.Column)
		if reason == "" && c.Category == "I" {
			reason = "network address type"
		}
		if reason == "" && o.Strict && strictType(c) {
			reason = "type " + c.Type
		}
		if reason == "" {
			continue
		}
		f := Finding{Schema: c.Schema, Table: c.Table, Column: c.Column, Reason: reason}
		if r, ok := rules.Get(c.Schema, c.Table, c.Column); ok {
			f.Kind = r.Kind
			f.Action = ActMasked
			if r.Kind == Keep {
				f.Action = ActKept
			}
		} else if c.Generated {
			f.Action = ActGenerated
		} else {
			f.Action = ActUnmasked
			unmasked = append(unmasked, f)
		}
		p.Findings = append(p.Findings, f)
	}
	if len(unmasked) > 0 && !o.AllowUnmasked {
		return nil, &UnmaskedError{Columns: unmasked}
	}

	for _, tu := range byTable {
		p.Tables = append(p.Tables, *tu)
	}
	sort.Slice(p.Tables, func(i, j int) bool {
		if p.Tables[i].Schema != p.Tables[j].Schema {
			return p.Tables[i].Schema < p.Tables[j].Schema
		}
		return p.Tables[i].Table < p.Tables[j].Table
	})
	return p, nil
}
