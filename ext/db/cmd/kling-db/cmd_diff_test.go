package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// diffKling contesta a las sesiones de psql de diff con un modelo: por
// máquina, el esquema (JSON fijo) y las filas de cada tabla (clave -> valores).
// Calcula las huellas como lo haría la SQL (sal de la propia consulta y muestreo
// por el primer octeto de la huella de clave), para probar la lógica del host.
type diffKling struct {
	*fakeKling
	schema  map[string]dbSchema            // por id de máquina
	rows    map[string]map[string]fakeRows // id -> tabla ("schema.name") -> filas
	scripts []string
}

type fakeRows map[string]string // clave primaria -> valores de la fila

var (
	reSalt  = regexp.MustCompile(`md5\('([0-9a-f]+)' \|\|`)
	reKStep = regexp.MustCompile(`kling-db:diff:rows k=(\d+)`)
	reFrom  = regexp.MustCompile(`FROM "((?:[^"]|"")*)"\."((?:[^"]|"")*)" AS t`)
)

func md5hex(s string) string { h := md5.Sum([]byte(s)); return hex.EncodeToString(h[:]) }

func (k *diffKling) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	if len(args) == 0 || args[0] != "exec" || stdin == nil {
		return k.fakeKling.Run(ctx, stdin, args...)
	}
	in, _ := io.ReadAll(stdin)
	sql := string(in)
	k.scripts = append(k.scripts, sql)
	id := args[indexOf(args, "--")-1]
	switch {
	case strings.Contains(sql, diffMarker+":schema"):
		b, _ := json.Marshal(k.schema[id])
		return append(b, '\n'), nil
	case strings.Contains(sql, diffMarker+":rows"):
		salt := reSalt.FindStringSubmatch(sql)[1]
		step, _ := strconv.Atoi(reKStep.FindStringSubmatch(sql)[1])
		m := reFrom.FindStringSubmatch(sql)
		name := strings.ReplaceAll(m[1], `""`, `"`) + "." + strings.ReplaceAll(m[2], `""`, `"`)
		var out strings.Builder
		for pk, row := range k.rows[id][name] {
			h := md5hex(salt + "(" + pk + ")")
			if step > 1 {
				v, _ := strconv.ParseUint(h[:8], 16, 64)
				if v%uint64(step) != 0 {
					continue
				}
			}
			fmt.Fprintf(&out, "R %s %s\n", h[:16], md5hex(row)[:16])
		}
		out.WriteString("DONE\n")
		return []byte(out.String()), nil
	}
	return nil, fmt.Errorf("diffKling: unexpected stdin %q", sql)
}

func i64(n int64) *int64 { return &n }

func tbl(name string, pk []string, count int64, extra ...dbCol) *dbTable {
	cols := append([]dbCol{{Name: "id", Type: "integer", NotNull: true}, {Name: "name", Type: "text"}}, extra...)
	return &dbTable{Schema: "public", Name: name, Cols: cols, PK: pk, Count: i64(count)}
}

func newDiffApp(t *testing.T) (*testApp, *diffKling, string, string) {
	t.Helper()
	ta := newTestApp(t)
	labels := func() map[string]string {
		return map[string]string{labelGolden: "pg", labelOwner: "local", labelState: stateReady}
	}
	m1 := ta.f.newMachine("before", labels())
	m2 := ta.f.newMachine("after", labels())
	k := &diffKling{fakeKling: ta.f, schema: map[string]dbSchema{}, rows: map[string]map[string]fakeRows{}}
	ta.app.k = k
	return ta, k, m1.ID, m2.ID
}

func TestDiffEsquemaYFilas(t *testing.T) {
	ta, k, a, b := newDiffApp(t)
	before := tbl("orders", []string{"id"}, 4)
	before.Indexes = []dbDef{{Name: "orders_name", Def: "CREATE INDEX orders_name ON public.orders (name)"}}
	before.Policies = []dbDef{{Name: "p", Def: "ALL PERMISSIVE TO public USING (tenant = 'acme') WITH CHECK ()"}}
	after := tbl("orders", []string{"id"}, 5, dbCol{Name: "note", Type: "text"})
	after.Cols[1].Type = "varchar(40)"
	after.RLS = true
	after.Policies = []dbDef{{Name: "p", Def: "ALL PERMISSIVE TO public USING (tenant = 'globex') WITH CHECK ()"}}
	after.Constraints = []dbDef{{Name: "orders_pk", Def: "PRIMARY KEY (id)"}}
	newT := tbl("audit", []string{"id"}, 7)
	oldT := tbl("legacy", nil, 2)
	k.schema[a] = dbSchema{Tables: []*dbTable{before, oldT}}
	k.schema[b] = dbSchema{Tables: []*dbTable{after, newT}}
	k.rows[a] = map[string]fakeRows{"public.orders": {"1": "a", "2": "b", "3": "c", "4": "d"}}
	k.rows[b] = map[string]fakeRows{"public.orders": {"1": "a", "2": "secret-B", "3": "c", "5": "secret-e"}}

	rep, err := ta.app.diff(context.Background(), "before", "after", "local", diffOpts{maxRows: 100})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Same {
		t.Fatal("should differ")
	}
	if len(rep.Schema.TablesAdded) != 1 || rep.Schema.TablesAdded[0] != "public.audit" ||
		len(rep.Schema.TablesRemoved) != 1 || rep.Schema.TablesRemoved[0] != "public.legacy" {
		t.Fatalf("tables: %+v", rep.Schema)
	}
	got := map[string]bool{}
	for _, c := range rep.Schema.Tables[0].Changes {
		got[c.Kind+" "+c.Name+" "+c.Change] = true
		if strings.Contains(c.Detail, "acme") || strings.Contains(c.Detail, "globex") {
			t.Errorf("literal in the report: %q", c.Detail)
		}
	}
	for _, want := range []string{"column note added", "column name changed", "rls  changed", "policy p changed", "constraint orders_pk added", "index orders_name removed"} {
		if !got[want] {
			t.Errorf("missing change %q in %v", want, got)
		}
	}
	var o *rowDiff
	for _, r := range rep.Rows {
		if r.Table == "public.orders" {
			o = r
		}
	}
	if o == nil || o.Status != "compared" || o.New != 1 || o.Deleted != 1 || o.Changed != 1 || o.Unchanged != 2 {
		t.Fatalf("rows: %+v", o)
	}
	// La sal es la misma en las dos copias y no es una clave ni un valor.
	var salts []string
	for _, s := range k.scripts {
		if m := reSalt.FindStringSubmatch(s); m != nil {
			salts = append(salts, m[1])
		}
		if strings.Contains(s, "secret") {
			t.Error("a value went into the SQL")
		}
	}
	if len(salts) != 2 || salts[0] != salts[1] || len(salts[0]) != 32 {
		t.Fatalf("salt: %v", salts)
	}
	// Texto y JSON.
	var buf strings.Builder
	if err := writeDiffReport(&buf, rep, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"+ table public.audit", "- table public.legacy", "new 1, deleted 1, changed 1, unchanged 2"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, buf.String())
		}
	}
	buf.Reset()
	if err := writeDiffReport(&buf, rep, true); err != nil || !json.Valid([]byte(buf.String())) {
		t.Fatalf("json: %v", err)
	}
}

func TestDiffIguales(t *testing.T) {
	ta, k, a, b := newDiffApp(t)
	for _, id := range []string{a, b} {
		k.schema[id] = dbSchema{Tables: []*dbTable{tbl("t", []string{"id"}, 2)}}
		k.rows[id] = map[string]fakeRows{"public.t": {"1": "x", "2": "y"}}
	}
	rep, err := ta.app.diff(context.Background(), "before", "after", "local", diffOpts{maxRows: 10})
	if err != nil || !rep.Same {
		t.Fatalf("same: %v %+v", err, rep)
	}
}

func TestDiffSoloEsquemaNoLeeFilas(t *testing.T) {
	ta, k, a, b := newDiffApp(t)
	for _, id := range []string{a, b} {
		k.schema[id] = dbSchema{Tables: []*dbTable{tbl("t", []string{"id"}, 2)}}
	}
	rep, err := ta.app.diff(context.Background(), "before", "after", "local", diffOpts{schemaOnly: true, maxRows: 10})
	if err != nil || !rep.Same || len(rep.Rows) != 0 {
		t.Fatalf("%v %+v", err, rep)
	}
	for _, s := range k.scripts {
		if strings.Contains(s, diffMarker+":rows") {
			t.Fatal("-schema-only read rows")
		}
		if strings.Contains(s, "count(*)") {
			t.Fatal("-schema-only counted rows")
		}
	}
}

func TestDiffSinClavePrimaria(t *testing.T) {
	ta, k, a, b := newDiffApp(t)
	k.schema[a] = dbSchema{Tables: []*dbTable{tbl("log", nil, 3), tbl("x", []string{"id"}, 1)}}
	k.schema[b] = dbSchema{Tables: []*dbTable{tbl("log", nil, 5), tbl("x", []string{"name"}, 1)}}
	rep, err := ta.app.diff(context.Background(), "before", "after", "local", diffOpts{maxRows: 10})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Same {
		t.Fatal("counts differ")
	}
	for _, r := range rep.Rows {
		if r.Status != "counts-only" || r.Note == "" {
			t.Errorf("%+v", r)
		}
	}
	for _, s := range k.scripts {
		if strings.Contains(s, diffMarker+":rows") {
			t.Fatal("rows read without a common primary key")
		}
	}
}

func TestDiffMuestreo(t *testing.T) {
	ta, k, a, b := newDiffApp(t)
	ra, rb := fakeRows{}, fakeRows{}
	for i := 0; i < 400; i++ {
		ra[strconv.Itoa(i)] = "v"
		rb[strconv.Itoa(i)] = "v"
	}
	rb["5"] = "changed"
	k.schema[a] = dbSchema{Tables: []*dbTable{tbl("big", []string{"id"}, 400)}}
	k.schema[b] = dbSchema{Tables: []*dbTable{tbl("big", []string{"id"}, 400)}}
	k.rows[a] = map[string]fakeRows{"public.big": ra}
	k.rows[b] = map[string]fakeRows{"public.big": rb}
	rep, err := ta.app.diff(context.Background(), "before", "after", "local", diffOpts{maxRows: 100})
	if err != nil {
		t.Fatal(err)
	}
	r := rep.Rows[0]
	if r.SampleEvery != 4 || r.Note == "" || r.Unchanged+r.Changed >= 400 || r.Unchanged+r.Changed < 20 {
		t.Fatalf("sampling: %+v", r)
	}
	if r.New != 0 || r.Deleted != 0 {
		t.Fatalf("the same rows must be sampled on both sides: %+v", r)
	}
}

// Los nombres vienen de la base: van citados.
func TestDiffCitaIdentificadores(t *testing.T) {
	tb := &dbTable{Schema: "s\"x", Name: "t'; DROP TABLE y; --", PK: []string{"i\"d"}, Cols: []dbCol{{Name: "i\"d"}, {Name: "n a"}}}
	sql := diffRowsSQL(tb, []string{"i\"d", "n a"}, "abcd", 1)
	for _, want := range []string{`"s""x"."t'; DROP TABLE y; --" AS t`, `t."i""d"`, `t."n a"`} {
		if !strings.Contains(sql, want) {
			t.Errorf("lacks %s:\n%s", want, sql)
		}
	}
	if !strings.Contains(sql, "default_transaction_read_only = on") {
		t.Error("not read-only")
	}
}

func TestDiffRechazos(t *testing.T) {
	ta, k, a, b := newDiffApp(t)
	k.schema[a] = dbSchema{}
	k.schema[b] = dbSchema{}
	if _, err := ta.app.diff(context.Background(), "before", "before", "local", diffOpts{maxRows: 1}); err == nil || !strings.Contains(err.Error(), "same copy") {
		t.Errorf("same copy: %v", err)
	}
	if _, err := ta.app.diff(context.Background(), "before", "after", "other", diffOpts{maxRows: 1}); err == nil || !strings.Contains(err.Error(), "belongs to owner") {
		t.Errorf("owner: %v", err)
	}
	if _, err := ta.app.diff(context.Background(), "before", "nope", "local", diffOpts{maxRows: 1}); err == nil {
		t.Error("missing copy")
	}
	if _, err := ta.app.diff(context.Background(), "before", "after", "local", diffOpts{maxRows: 0}); err == nil {
		t.Error("max-rows 0")
	}
	ta.f.find("after").State = "warm"
	if _, err := ta.app.diff(context.Background(), "before", "after", "local", diffOpts{maxRows: 1}); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("state: %v", err)
	}
}
