package main

// `kling db diff <copia1> <copia2>`: qué cambió entre dos copias, sin volcar
// datos. Complementa rehearse y undo: "esta migración añadió 3 columnas y
// cambió 1.200 filas".
//
// Todo por `kling exec` + psql por stdin, dentro de cada copia:
//
//  1. Esquema (una consulta por copia, JSON): tablas, columnas y tipos, clave
//     primaria, índices, restricciones, políticas RLS y, salvo con
//     -schema-only, el recuento exacto de cada tabla. Se compara en el host.
//  2. Filas (una sesión por tabla y copia, solo tablas con la MISMA clave
//     primaria en ambas): por fila, dos huellas de 64 bits, la de la clave
//     (md5 con una sal aleatoria de esta ejecución, igual en las dos copias)
//     y la de la fila (md5 de sus columnas comunes). Al host solo llegan
//     huellas: ni una clave ni un valor salen de la base. La sal impide
//     buscar claves conocidas (ids pequeños) en las huellas.
//
// Filas nuevas / borradas / cambiadas se calculan cruzando las huellas de
// clave. Tablas sin clave primaria (o con otra distinta en cada copia): solo
// recuentos y un aviso. Con más de -max-rows filas en una tabla se muestrea:
// las filas cuya huella de clave cae en 1 de cada k (el mismo criterio en las
// dos copias, así que se comparan las mismas filas) y el informe lo declara.
//
// Sin inyección: los nombres de tablas y columnas vienen de la base y van
// citados como identificadores (qIdent); la sal es hexadecimal y el resto son
// números que fija este proceso. Las definiciones que se enseñan pasan por
// sinLiterales: el informe enseña estructura, no valores. Las sesiones son de
// solo lectura. Las copias deberían estar quietas mientras se comparan: cada
// tabla se lee en su propia sesión.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/juan52878911/kindling/ext/db/internal/doctor"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/plugin"
)

const (
	diffMarker       = "kling-db:diff" // va en lo que se manda (los tests lo buscan)
	defaultDiffRows  = 100000
	maxDiffRows      = 2000000
	maxDiffTables    = 500
	diffSchemaTmo    = "300s"
	diffRowsTmo      = "600s"
	diffStmtTmo      = "300s"
	diffRowsStmtTmo  = "600s"
	diffDetailMaxLen = 300
)

type diffOpts struct {
	schemaOnly bool
	maxRows    int
}

func (o diffOpts) validate() error {
	if o.maxRows < 1 || o.maxRows > maxDiffRows {
		return fmt.Errorf("-max-rows must be between 1 and %d", maxDiffRows)
	}
	return nil
}

// ── esquema de una copia ────────────────────────────────────────────────────

type dbSchema struct {
	Tables []*dbTable `json:"tables"`
}

type dbTable struct {
	Schema      string   `json:"schema"`
	Name        string   `json:"name"`
	RLS         bool     `json:"rls"`
	Force       bool     `json:"force"`
	Cols        []dbCol  `json:"cols"`
	PK          []string `json:"pk"`
	Indexes     []dbDef  `json:"indexes"`
	Constraints []dbDef  `json:"constraints"`
	Policies    []dbDef  `json:"policies"`
	Count       *int64   `json:"count"`
	byCol       map[string]dbCol
}

type dbCol struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	NotNull bool   `json:"notnull"`
	Default string `json:"default"`
}

type dbDef struct {
	Name string `json:"name"`
	Def  string `json:"def"`
}

func (t *dbTable) key() string { return t.Schema + "." + t.Name }

func (t *dbTable) display() string { return doctor.Safe(t.Schema, 64) + "." + doctor.Safe(t.Name, 64) }

// diffSchemaSQL: sin datos, solo el catálogo. counts pide el recuento exacto
// de cada tabla (query_to_xml, con el nombre citado por %I).
func diffSchemaSQL(counts bool) string {
	count := "NULL::bigint"
	if counts {
		count = "(xpath('/row/c/text()', query_to_xml(format('SELECT count(*) AS c FROM %I.%I', n.nspname, c.relname), false, true, '')))[1]::text::bigint"
	}
	return fmt.Sprintf(`-- `+diffMarker+`:schema
SET default_transaction_read_only = on;
SET statement_timeout = '`+diffStmtTmo+`';
SELECT jsonb_build_object('tables', coalesce((SELECT jsonb_agg(t ORDER BY t.schema, t.name) FROM (
  SELECT n.nspname::text AS schema, c.relname::text AS name,
    c.relrowsecurity AS rls, c.relforcerowsecurity AS force,
    (SELECT coalesce(jsonb_agg(jsonb_build_object('name', a.attname::text,
        'type', format_type(a.atttypid, a.atttypmod), 'notnull', a.attnotnull,
        'default', coalesce(pg_get_expr(d.adbin, d.adrelid), '')) ORDER BY a.attnum), '[]'::jsonb)
      FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
      WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped) AS cols,
    (SELECT coalesce(jsonb_agg(a.attname::text ORDER BY k.ord), '[]'::jsonb)
      FROM pg_index i
      CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
      JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
      WHERE i.indrelid = c.oid AND i.indisprimary) AS pk,
    (SELECT coalesce(jsonb_agg(jsonb_build_object('name', ic.relname::text, 'def', pg_get_indexdef(i.indexrelid)) ORDER BY ic.relname), '[]'::jsonb)
      FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid WHERE i.indrelid = c.oid) AS indexes,
    (SELECT coalesce(jsonb_agg(jsonb_build_object('name', k.conname::text, 'def', pg_get_constraintdef(k.oid)) ORDER BY k.conname), '[]'::jsonb)
      FROM pg_constraint k WHERE k.conrelid = c.oid) AS constraints,
    (SELECT coalesce(jsonb_agg(jsonb_build_object('name', p.policyname::text,
        'def', format('%%s %%s TO %%s USING (%%s) WITH CHECK (%%s)', p.cmd, p.permissive,
          array_to_string(p.roles::text[], ','), coalesce(p.qual, ''), coalesce(p.with_check, ''))) ORDER BY p.policyname), '[]'::jsonb)
      FROM pg_policies p WHERE p.schemaname = n.nspname AND p.tablename = c.relname) AS policies,
    %s AS count
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
  WHERE c.relkind IN ('r', 'p') AND NOT c.relispartition
    AND n.nspname <> 'information_schema' AND n.nspname !~ '^pg_'
    AND NOT EXISTS (SELECT 1 FROM pg_depend e WHERE e.classid = 'pg_class'::regclass AND e.objid = c.oid AND e.deptype = 'e')
) t), '[]'::jsonb))::text;
`, count)
}

// diffRowsSQL: las huellas de las filas de una tabla. cols (ya ordenadas) y pk
// son nombres de la base: van citados. salt es hexadecimal; k es 1 (todas las
// filas) o el paso del muestreo.
func diffRowsSQL(t *dbTable, cols []string, salt string, k int64) string {
	q := func(names []string) string {
		out := make([]string, len(names))
		for i, n := range names {
			out[i] = "t." + qIdent(n)
		}
		return strings.Join(out, ", ")
	}
	return fmt.Sprintf(`-- `+diffMarker+`:rows k=%[1]d
SET default_transaction_read_only = on;
SET statement_timeout = '`+diffRowsStmtTmo+`';
SELECT 'R ' || left(h, 16) || ' ' || left(r, 16) FROM (
  SELECT md5('%[2]s' || ROW(%[3]s)::text) AS h, md5(ROW(%[4]s)::text) AS r
  FROM %[5]s.%[6]s AS t
) s
WHERE %[1]d = 1 OR mod(('x' || left(h, 8))::bit(32)::bigint, %[1]d) = 0;
\echo DONE
`, k, salt, q(t.PK), q(cols), qIdent(t.Schema), qIdent(t.Name))
}

// ── informe ─────────────────────────────────────────────────────────────────

type diffReport struct {
	From       string        `json:"from"`
	To         string        `json:"to"`
	SchemaOnly bool          `json:"schema_only"`
	MaxRows    int           `json:"max_rows"`
	Schema     diffSchemaRep `json:"schema"`
	Rows       []*rowDiff    `json:"rows"`
	Same       bool          `json:"same"` // sin diferencias de esquema ni de filas
}

type diffSchemaRep struct {
	TablesAdded   []string     `json:"tables_added"` // en la segunda y no en la primera
	TablesRemoved []string     `json:"tables_removed"`
	Tables        []*tableDiff `json:"tables"` // tablas comunes que cambiaron
}

type tableDiff struct {
	Table   string       `json:"table"`
	Changes []diffChange `json:"changes"`
}

type diffChange struct {
	Kind   string `json:"kind"`   // column | primary-key | index | constraint | policy | rls
	Name   string `json:"name"`   // vacío en primary-key y rls
	Change string `json:"change"` // added | removed | changed
	Detail string `json:"detail,omitempty"`
}

type rowDiff struct {
	Table       string `json:"table"`
	Status      string `json:"status"` // compared | counts-only | added | removed
	Rows1       int64  `json:"rows_from"`
	Rows2       int64  `json:"rows_to"`
	New         int64  `json:"new"`
	Deleted     int64  `json:"deleted"`
	Changed     int64  `json:"changed"`
	Unchanged   int64  `json:"unchanged"`
	SampleEvery int64  `json:"sample_every,omitempty"` // >1: solo 1 de cada k filas (por huella de clave)
	Note        string `json:"note,omitempty"`
}

// ── comando ─────────────────────────────────────────────────────────────────

func cmdDiff(args []string) error {
	fs, host, owner := newFlags("diff")
	asJSON := fs.Bool("json", false, "JSON output")
	schemaOnly := fs.Bool("schema-only", false, "compare only the schema, not the rows")
	maxRows := fs.Int("max-rows", defaultDiffRows, fmt.Sprintf("rows fingerprinted per table before sampling (1-%d)", maxDiffRows))
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageErr("usage: kling db diff <copy1> <copy2> [-json] [-schema-only] [-max-rows N]")
	}
	o := diffOpts{schemaOnly: *schemaOnly, maxRows: *maxRows}
	if err := o.validate(); err != nil {
		return &plugin.ExitError{Code: 2, Err: err}
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	rep, err := a.diff(ctx, pos[0], pos[1], *owner, o)
	if err != nil {
		return err
	}
	return writeDiffReport(a.stdout, rep, *asJSON)
}

// diffSide es una copia lista para leer.
type diffSide struct {
	mc     *api.Machine
	db     string
	schema *dbSchema
	tables map[string]*dbTable
}

func (a *app) diffSide(ctx context.Context, ref, owner string, o diffOpts) (*diffSide, error) {
	mc, err := a.inspect(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := owned(mc, owner); err != nil {
		return nil, err
	}
	if mc.State != api.StateRunning {
		return nil, fmt.Errorf("%s is %s, not running (kling thaw %s)", mc.Name, mc.State, mc.Name)
	}
	_, db, err := roleDB(mc.Labels)
	if err != nil {
		return nil, err
	}
	s := &diffSide{mc: mc, db: db}
	out, err := a.diffExec(ctx, s, diffSchemaSQL(!o.schemaOnly), diffSchemaTmo)
	if err != nil {
		return nil, fmt.Errorf("diff: reading the schema of %s: %w", mc.Name, err)
	}
	var sc dbSchema
	if err := json.Unmarshal([]byte(lastLine(string(out))), &sc); err != nil {
		return nil, fmt.Errorf("diff: unexpected schema output from %s: %w", mc.Name, err)
	}
	if len(sc.Tables) > maxDiffTables {
		return nil, fmt.Errorf("%s has %d tables; diff handles at most %d", mc.Name, len(sc.Tables), maxDiffTables)
	}
	s.schema = &sc
	s.tables = map[string]*dbTable{}
	for _, t := range sc.Tables {
		t.byCol = map[string]dbCol{}
		for _, c := range t.Cols {
			t.byCol[c.Name] = c
		}
		s.tables[t.key()] = t
	}
	return s, nil
}

// diffExec manda sql por stdin al psql (postgres, solo lectura) de la copia.
func (a *app) diffExec(ctx context.Context, s *diffSide, sql, timeout string) ([]byte, error) {
	out, err := a.k.Run(ctx, strings.NewReader(sql), "exec", "-i", "-timeout", timeout, s.mc.ID, "--",
		"su", "-s", "/bin/sh", "postgres", "-c", psqlSuperDB(s.db))
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > diffMaxOut {
		return nil, errors.New("output too large")
	}
	return out, nil
}

// diffMaxOut acota lo que se lee de una sesión (maxDiffRows huellas de ~40 B).
const diffMaxOut = 128 << 20

// diff compara las copias r1 y r2 (de owner). err es para lo que impide
// comparar; las diferencias van en el informe.
func (a *app) diff(ctx context.Context, r1, r2, owner string, o diffOpts) (*diffReport, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	s1, err := a.diffSide(ctx, r1, owner, o)
	if err != nil {
		return nil, err
	}
	s2, err := a.diffSide(ctx, r2, owner, o)
	if err != nil {
		return nil, err
	}
	if s1.mc.ID == s2.mc.ID {
		return nil, errors.New("diff: both arguments are the same copy")
	}
	rep := &diffReport{From: s1.mc.Name, To: s2.mc.Name, SchemaOnly: o.schemaOnly, MaxRows: o.maxRows, Rows: []*rowDiff{},
		Schema: diffSchemaRep{TablesAdded: []string{}, TablesRemoved: []string{}, Tables: []*tableDiff{}}}

	// Esquema.
	var common []string
	for _, t := range s1.schema.Tables {
		if _, ok := s2.tables[t.key()]; ok {
			common = append(common, t.key())
		} else {
			rep.Schema.TablesRemoved = append(rep.Schema.TablesRemoved, t.display())
		}
	}
	for _, t := range s2.schema.Tables {
		if _, ok := s1.tables[t.key()]; !ok {
			rep.Schema.TablesAdded = append(rep.Schema.TablesAdded, t.display())
		}
	}
	for _, k := range common {
		if ch := diffTable(s1.tables[k], s2.tables[k]); len(ch) > 0 {
			rep.Schema.Tables = append(rep.Schema.Tables, &tableDiff{Table: s1.tables[k].display(), Changes: ch})
		}
	}
	if !o.schemaOnly {
		if err := a.diffRows(ctx, rep, s1, s2, common, o); err != nil {
			return nil, err
		}
	}
	rep.Same = len(rep.Schema.TablesAdded) == 0 && len(rep.Schema.TablesRemoved) == 0 && len(rep.Schema.Tables) == 0
	for _, r := range rep.Rows {
		if r.New+r.Deleted+r.Changed > 0 || (r.Status == "counts-only" && r.Rows1 != r.Rows2) {
			rep.Same = false
		}
	}
	return rep, nil
}

// ── esquema: comparación ────────────────────────────────────────────────────

func diffTable(a, b *dbTable) []diffChange {
	var out []diffChange
	if a.RLS != b.RLS || a.Force != b.Force {
		out = append(out, diffChange{Kind: "rls", Change: "changed",
			Detail: fmt.Sprintf("row level security %s -> %s", onOff(a.RLS, a.Force), onOff(b.RLS, b.Force))})
	}
	if !equalStrings(a.PK, b.PK) {
		out = append(out, diffChange{Kind: "primary-key", Change: "changed",
			Detail: fmt.Sprintf("(%s) -> (%s)", safeList(a.PK), safeList(b.PK))})
	}
	// Columnas, por nombre.
	for _, c := range a.Cols {
		d, ok := b.byCol[c.Name]
		switch {
		case !ok:
			out = append(out, diffChange{Kind: "column", Name: doctor.Safe(c.Name, 64), Change: "removed", Detail: c.Type})
		case c != d:
			var what []string
			if c.Type != d.Type {
				what = append(what, fmt.Sprintf("type %s -> %s", doctor.Safe(c.Type, 80), doctor.Safe(d.Type, 80)))
			}
			if c.NotNull != d.NotNull {
				what = append(what, fmt.Sprintf("not null %t -> %t", c.NotNull, d.NotNull))
			}
			if c.Default != d.Default {
				what = append(what, "default changed")
			}
			out = append(out, diffChange{Kind: "column", Name: doctor.Safe(c.Name, 64), Change: "changed", Detail: strings.Join(what, "; ")})
		}
	}
	for _, d := range b.Cols {
		if _, ok := a.byCol[d.Name]; !ok {
			out = append(out, diffChange{Kind: "column", Name: doctor.Safe(d.Name, 64), Change: "added", Detail: doctor.Safe(d.Type, 80)})
		}
	}
	out = append(out, diffDefs("index", a.Indexes, b.Indexes)...)
	out = append(out, diffDefs("constraint", a.Constraints, b.Constraints)...)
	out = append(out, diffDefs("policy", a.Policies, b.Policies)...)
	return out
}

func diffDefs(kind string, a, b []dbDef) []diffChange {
	am, bm := map[string]string{}, map[string]string{}
	for _, d := range a {
		am[d.Name] = d.Def
	}
	for _, d := range b {
		bm[d.Name] = d.Def
	}
	show := func(s string) string { return doctor.Safe(sinLiterales(s), diffDetailMaxLen) }
	var out []diffChange
	for _, d := range a {
		nb, ok := bm[d.Name]
		switch {
		case !ok:
			out = append(out, diffChange{Kind: kind, Name: doctor.Safe(d.Name, 64), Change: "removed", Detail: show(d.Def)})
		case nb != d.Def:
			out = append(out, diffChange{Kind: kind, Name: doctor.Safe(d.Name, 64), Change: "changed",
				Detail: show(d.Def) + " -> " + show(nb)})
		}
	}
	for _, d := range b {
		if _, ok := am[d.Name]; !ok {
			out = append(out, diffChange{Kind: kind, Name: doctor.Safe(d.Name, 64), Change: "added", Detail: show(d.Def)})
		}
	}
	return out
}

func onOff(rls, force bool) string {
	switch {
	case rls && force:
		return "on (forced)"
	case rls:
		return "on"
	}
	return "off"
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func safeList(s []string) string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = doctor.Safe(v, 64)
	}
	return strings.Join(out, ", ")
}

// ── filas: huellas ──────────────────────────────────────────────────────────

func (a *app) diffRows(ctx context.Context, rep *diffReport, s1, s2 *diffSide, common []string, o diffOpts) error {
	count := func(t *dbTable) int64 {
		if t.Count == nil {
			return 0
		}
		return *t.Count
	}
	// Tablas que solo están en una: su recuento.
	for _, t := range s1.schema.Tables {
		if _, ok := s2.tables[t.key()]; !ok {
			rep.Rows = append(rep.Rows, &rowDiff{Table: t.display(), Status: "removed", Rows1: count(t), Deleted: count(t)})
		}
	}
	for _, t := range s2.schema.Tables {
		if _, ok := s1.tables[t.key()]; !ok {
			rep.Rows = append(rep.Rows, &rowDiff{Table: t.display(), Status: "added", Rows2: count(t), New: count(t)})
		}
	}
	var salt string
	for _, k := range common {
		t1, t2 := s1.tables[k], s2.tables[k]
		rd := &rowDiff{Table: t1.display(), Rows1: count(t1), Rows2: count(t2)}
		rep.Rows = append(rep.Rows, rd)
		switch {
		case len(t1.PK) == 0 || len(t2.PK) == 0:
			rd.Status = "counts-only"
			rd.Note = "no primary key: only row counts are compared"
			continue
		case !equalStrings(t1.PK, t2.PK):
			rd.Status = "counts-only"
			rd.Note = "the primary key differs between the copies: only row counts are compared"
			continue
		}
		cols := commonCols(t1, t2)
		if len(cols) == 0 {
			rd.Status = "counts-only"
			rd.Note = "no columns in common: only row counts are compared"
			continue
		}
		if salt == "" {
			var b [16]byte
			if _, err := rand.Read(b[:]); err != nil {
				return err
			}
			salt = hex.EncodeToString(b[:])
		}
		step := int64(1)
		if n := max(rd.Rows1, rd.Rows2); n > int64(o.maxRows) {
			step = (n + int64(o.maxRows) - 1) / int64(o.maxRows)
			rd.SampleEvery = step
			rd.Note = fmt.Sprintf("sampled: 1 of every %d rows by key hash (over -max-rows %d); counts below are of the sample", step, o.maxRows)
		}
		limit := int64(o.maxRows)*2 + 1000 // el recuento pudo crecer entre las dos lecturas
		f1, err := a.fingerprints(ctx, s1, t1, cols, salt, step, limit)
		if err != nil {
			return err
		}
		f2, err := a.fingerprints(ctx, s2, t2, cols, salt, step, limit)
		if err != nil {
			return err
		}
		rd.Status = "compared"
		for pk, r := range f1 {
			r2, ok := f2[pk]
			switch {
			case !ok:
				rd.Deleted++
			case r2 != r:
				rd.Changed++
			default:
				rd.Unchanged++
			}
		}
		for pk := range f2 {
			if _, ok := f1[pk]; !ok {
				rd.New++
			}
		}
	}
	sort.SliceStable(rep.Rows, func(i, j int) bool { return rep.Rows[i].Table < rep.Rows[j].Table })
	return nil
}

// commonCols son las columnas de las dos tablas por nombre, ordenadas: una
// columna añadida no debe cambiar la huella de todas las filas.
func commonCols(a, b *dbTable) []string {
	var cols []string
	for _, c := range a.Cols {
		if _, ok := b.byCol[c.Name]; ok {
			cols = append(cols, c.Name)
		}
	}
	sort.Strings(cols)
	return cols
}

var reFingerprint = regexp.MustCompile(`^R ([0-9a-f]{16}) ([0-9a-f]{16})$`)

// fingerprints devuelve huella de clave -> huella de fila de una tabla.
func (a *app) fingerprints(ctx context.Context, s *diffSide, t *dbTable, cols []string, salt string, step, limit int64) (map[string]string, error) {
	out, err := a.diffExec(ctx, s, diffRowsSQL(t, cols, salt, step), diffRowsTmo)
	if err != nil {
		return nil, fmt.Errorf("diff: reading rows of %s in %s: %w", t.display(), s.mc.Name, err)
	}
	m := map[string]string{}
	done := false
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 0, 4096), 4096)
	var n int64
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "DONE" {
			done = true
			continue
		}
		mm := reFingerprint.FindStringSubmatch(line)
		if mm == nil {
			return nil, fmt.Errorf("diff: unexpected output reading rows of %s in %s", t.display(), s.mc.Name)
		}
		if n++; n > limit {
			return nil, fmt.Errorf("diff: %s in %s changed while it was read (more rows than counted)", t.display(), s.mc.Name)
		}
		m[mm[1]] = mm[2]
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !done {
		return nil, fmt.Errorf("diff: reading rows of %s in %s did not finish", t.display(), s.mc.Name)
	}
	return m, nil
}

// ── salida ──────────────────────────────────────────────────────────────────

func writeDiffReport(w io.Writer, rep *diffReport, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	fmt.Fprintf(w, "kling db diff: %s -> %s\n", doctor.Safe(rep.From, 64), doctor.Safe(rep.To, 64))
	fmt.Fprintln(w, "schema:")
	s := rep.Schema
	if len(s.TablesAdded)+len(s.TablesRemoved)+len(s.Tables) == 0 {
		fmt.Fprintln(w, "  no differences")
	}
	for _, t := range s.TablesAdded {
		fmt.Fprintf(w, "  + table %s\n", t)
	}
	for _, t := range s.TablesRemoved {
		fmt.Fprintf(w, "  - table %s\n", t)
	}
	sym := map[string]string{"added": "+", "removed": "-", "changed": "~"}
	for _, t := range s.Tables {
		fmt.Fprintf(w, "  ~ %s\n", t.Table)
		for _, c := range t.Changes {
			line := fmt.Sprintf("      %s %s", sym[c.Change], c.Kind)
			if c.Name != "" {
				line += " " + c.Name
			}
			if c.Detail != "" {
				line += ": " + c.Detail
			}
			fmt.Fprintln(w, line)
		}
	}
	if rep.SchemaOnly {
		fmt.Fprintln(w, "rows: not compared (-schema-only)")
	} else {
		fmt.Fprintln(w, "rows (compared by fingerprints: no row or key leaves the copies):")
		if len(rep.Rows) == 0 {
			fmt.Fprintln(w, "  no tables")
		}
		var nNew, nDel, nChg int64
		for _, r := range rep.Rows {
			switch r.Status {
			case "added":
				fmt.Fprintf(w, "  %-30s new table, %d rows\n", r.Table, r.Rows2)
			case "removed":
				fmt.Fprintf(w, "  %-30s dropped table, had %d rows\n", r.Table, r.Rows1)
			case "counts-only":
				fmt.Fprintf(w, "  %-30s rows %d -> %d  (warning: %s)\n", r.Table, r.Rows1, r.Rows2, r.Note)
			default:
				fmt.Fprintf(w, "  %-30s new %d, deleted %d, changed %d, unchanged %d  (rows %d -> %d)\n",
					r.Table, r.New, r.Deleted, r.Changed, r.Unchanged, r.Rows1, r.Rows2)
				if r.Note != "" {
					fmt.Fprintf(w, "  %-30s note: %s\n", "", r.Note)
				}
				nNew, nDel, nChg = nNew+r.New, nDel+r.Deleted, nChg+r.Changed
			}
		}
		fmt.Fprintf(w, "rows in tables compared by key: %d new, %d deleted, %d changed\n", nNew, nDel, nChg)
	}
	if rep.Same {
		fmt.Fprintln(w, "summary: identical")
	} else {
		fmt.Fprintf(w, "summary: %d table(s) added, %d dropped, %d changed in the schema\n",
			len(s.TablesAdded), len(s.TablesRemoved), len(s.Tables))
	}
	return nil
}
