package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/ext/db/internal/dbmask"
)

// Catálogo de producción de las pruebas: orders (clave id) apunta a users por
// user_id; order_items apunta a orders por order_id; audit apunta a orders dos
// veces y además orders le apunta a ella (padre e hija a la vez).
func sliceDiscoverOut() string {
	j := func(f ...string) string { return strings.Join(f, "\x1f") }
	return strings.Join([]string{
		j("target", "r", `public.orders`, "public.orders", `id,user_id,total`, "id"),
		j("parent", "r", `public.users`, "public.users", `id,email,full_name`, "user_id", "id"),
		j("child", "p", `public.order_items`, "public.order_items", `id,order_id,qty`, "order_id", "id"),
		j("child", "r", `public."Audit Log"`, `public.Audit Log`, `id,order_id,prev_order`, "order_id", "id"),
		j("child", "r", `public."Audit Log"`, `public.Audit Log`, `id,order_id,prev_order`, "prev_order", "id"),
	}, "\n") + "\n"
}

func sliceTest(t *testing.T) (*cloneTest, cloneOpts) {
	t.Helper()
	ct := newCloneTest(t)
	ct.f.discover = sliceDiscoverOut()
	ct.f.fixup = "sl|fk|notvalid|users.users_org_fk\nsl|rows|0|1000\nsl|rows|1|12\nsl|rows|2|40\nsl|rows|3|7\n"
	o := ct.opts(t, cloneRules)
	o.slice = &sliceSpec{table: "public.orders", rows: 1000}
	if err := o.slice.validate(); err != nil {
		t.Fatal(err)
	}
	return ct, o
}

func TestSliceBien(t *testing.T) {
	ct, o := sliceTest(t)
	rep, err := ct.c.run(context.Background(), o)
	if err != nil {
		t.Fatalf("%v\n%s", err, ct.out)
	}
	ct.sinRastro(t)
	// El volcado entero NO se hace; el slice sí, con su guion.
	var fill, discover string
	for _, c := range ct.f.calls {
		switch {
		case strings.Contains(c.stdin, "kling-db:clone-dump"):
			t.Fatal("slice dumped the whole source")
		case strings.Contains(c.stdin, sliceFillMarker):
			fill = c.stdin
		case strings.Contains(c.stdin, sliceDiscoverMarker):
			discover = c.stdin
		}
	}
	if !strings.Contains(discover, "to_regclass('public.orders')") || !strings.Contains(discover, "READ ONLY") {
		t.Fatalf("discover script:\n%s", discover)
	}
	for _, want := range []string{
		"--schema-only",
		"BEGIN ISOLATION LEVEL REPEATABLE READ, READ ONLY;",
		`\copy (SELECT id,user_id,total FROM ONLY public.orders ORDER BY id LIMIT 1000) TO PROGRAM 'sh /run/klingclone/slice-load.sh 0'`,
		"SET session_replication_role = replica;",
		`\copy public."Audit Log" (id,order_id,prev_order) FROM pstdin`,
	} {
		if !strings.Contains(fill, want) {
			t.Errorf("the fill script lacks %q", want)
		}
	}
	// Ni un fichero con datos: nada de COPY a fichero ni de pg_dump con -f.
	if strings.Contains(fill, " TO '") || strings.Contains(fill, "pg_dump -f") {
		t.Error("the fill script writes data to a file")
	}
	if ct.c.sliceRes == nil || len(ct.c.sliceRes.Loaded) != 4 || ct.c.sliceRes.Loaded[3].Rows != 7 ||
		ct.c.sliceRes.Loaded[3].Role != "child" || ct.c.sliceRes.Loaded[1].Role != "parent" {
		t.Fatalf("slice result: %+v", ct.c.sliceRes)
	}
	if len(ct.c.sliceRes.NotValidFKs) != 1 || ct.c.sliceRes.NotValidFKs[0] != "users.users_org_fk" {
		t.Fatalf("not valid: %+v", ct.c.sliceRes.NotValidFKs)
	}
	if len(ct.golden) != 1 {
		t.Fatalf("golden builds: %v", ct.golden)
	}

	// El informe: el del enmascarado, el del slice y el aviso de los tiempos.
	var buf bytes.Buffer
	ct.c.sliceRes.Table, ct.c.sliceRes.RowsLimit, ct.c.sliceRes.Warning = "public.orders", 1000, statsWarning
	if err := writeSliceReport(&buf, rep, ct.c.sliceRes, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"slice of public.orders", "NOT VALID: users.users_org_fk", "pg_restore_relation_stats", "every other table: empty"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, buf.String())
		}
	}
	buf.Reset()
	if err := writeSliceReport(&buf, rep, ct.c.sliceRes, true); err != nil {
		t.Fatal(err)
	}
	var js map[string]any
	if err := json.Unmarshal(buf.Bytes(), &js); err != nil || js["slice"] == nil || js["golden"] != "shop-masked" {
		t.Fatalf("json report: %v %s", err, buf.String())
	}

	// El fichero del slice, para observe.
	if err := writeSliceMeta("shop-masked", o.source.String(), ct.c.sliceRes); err != nil {
		t.Fatal(err)
	}
	m, err := readSliceMeta("shop-masked")
	if err != nil || m == nil || m.Table != "public.orders" || m.Rows != 1000 {
		t.Fatalf("slice meta: %+v %v", m, err)
	}
	st, err := os.Stat(filepath.Join(os.Getenv("KLING_DB_STATE"), "shop-masked", sliceFile))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("slice.json: %v %v", st, err)
	}
	if m, err := readSliceMeta("otro"); m != nil || err != nil {
		t.Fatalf("missing slice.json: %v %v", m, err)
	}
}

// Si la extracción falla, no queda golden ni máquina.
func TestSliceFalloNoDejaGolden(t *testing.T) {
	ct, o := sliceTest(t)
	ct.f.fillErr = errors.New("extracting the slice from the source failed")
	if _, err := ct.c.run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "copying the slice") {
		t.Fatalf("err = %v", err)
	}
	ct.sinRastro(t)
	if len(ct.golden) != 0 {
		t.Fatal("a golden was built")
	}
	for _, c := range ct.f.calls {
		if strings.Contains(c.stdin, dbmask.MaskMarker) {
			t.Fatal("masked after a failed slice")
		}
	}
}

func TestSliceTablaQueNoExiste(t *testing.T) {
	ct, o := sliceTest(t)
	ct.f.discover = ""
	if _, err := ct.c.run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v", err)
	}
	ct.sinRastro(t)
}

func TestSliceSpec(t *testing.T) {
	for _, c := range []struct {
		s  sliceSpec
		ok bool
	}{
		{sliceSpec{table: "orders", rows: 10}, true},
		{sliceSpec{table: "public.orders", rows: 10, related: 5}, true},
		{sliceSpec{table: "a$b", rows: 1}, true},
		{sliceSpec{table: `"Orders"`, rows: 10}, false},
		{sliceSpec{table: "a.b.c", rows: 10}, false},
		{sliceSpec{table: "orders; DROP TABLE x", rows: 10}, false},
		{sliceSpec{table: "o'rders", rows: 10}, false},
		{sliceSpec{table: "orders", rows: 0}, false},
		{sliceSpec{table: "orders", rows: sliceMaxRows + 1}, false},
		{sliceSpec{table: "orders", rows: 10, related: -1}, false},
	} {
		err := c.s.validate()
		if (err == nil) != c.ok {
			t.Errorf("%+v: %v", c.s, err)
		}
	}
	s := sliceSpec{table: "orders", rows: 7}
	if s.validate(); s.related != 7 {
		t.Errorf("related default = %d", s.related)
	}
	for db, want := range map[string]string{"shop": "shop-orders-slice"} {
		if got := defaultSliceGolden(db, "public.orders"); got != want || !goldenNamePattern.MatchString(got) {
			t.Errorf("defaultSliceGolden = %q", got)
		}
	}
	if got := defaultSliceGolden(strings.Repeat("x", 60), "t"); !goldenNamePattern.MatchString(got) {
		t.Errorf("long name %q", got)
	}
}

func TestParseSliceDiscover(t *testing.T) {
	tg, rels, err := parseSliceDiscover([]byte(sliceDiscoverOut()))
	if err != nil || tg.qname != "public.orders" || tg.pk != "id" || len(rels) != 4 {
		t.Fatalf("%+v %+v %v", tg, rels, err)
	}
	j := func(f ...string) string { return strings.Join(f, "\x1f") + "\n" }
	for name, out := range map[string]string{
		"sin tabla":    "",
		"vista":        j("target", "v", "public.v", "public.v", "a", ""),
		"sin columnas": j("target", "r", "public.t", "public.t", "", ""),
		"control":      j("target", "r", "public.t", "public.t", "a\x1bb", ""),
		"barra":        j("target", "r", `public."a\b"`, `public.a\b`, "a", ""),
		"dos tablas":   j("target", "r", "public.t", "public.t", "a", "") + j("target", "r", "public.u", "public.u", "a", ""),
		"rara":         j("otra", "r", "x"),
		"fk vacía":     j("target", "r", "public.t", "public.t", "a", "") + j("parent", "r", "public.u", "public.u", "a", "", "id"),
	} {
		if _, _, err := parseSliceDiscover([]byte(out)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var many strings.Builder
	many.WriteString(j("target", "r", "public.t", "public.t", "a", "id"))
	for range sliceMaxRelations + 1 {
		many.WriteString(j("child", "r", "public.c", "public.c", "a", "t_id", "id"))
	}
	if _, _, err := parseSliceDiscover([]byte(many.String())); err == nil {
		t.Error("too many relations accepted")
	}
}

// La planificación: misma muestra en todas las consultas, grupos por tabla,
// sin la propia tabla (clave a sí misma) y orden estable sin clave primaria.
func TestPlanSlice(t *testing.T) {
	tg, rels, _ := parseSliceDiscover([]byte(sliceDiscoverOut()))
	rels = append(rels, sliceRel{dir: "parent", other: tg, fk: "parent_id", ref: "id"}) // a sí misma
	loads := planSlice(tg, rels, &sliceSpec{table: "public.orders", rows: 50, related: 9})
	if len(loads) != 4 {
		t.Fatalf("loads: %+v", loads)
	}
	sample := "ORDER BY id LIMIT 50"
	for _, l := range loads {
		if !strings.Contains(l.query, sample) || strings.Contains(l.query, "\n") {
			t.Errorf("%s: %s", l.table.plain, l.query)
		}
	}
	if want := "SELECT id,email,full_name FROM ONLY public.users WHERE (tableoid, ctid) IN (SELECT tableoid, ctid FROM ONLY public.users WHERE (id) IN (SELECT user_id FROM ONLY public.orders ORDER BY id LIMIT 50))"; loads[1].query != want {
		t.Errorf("parent query:\n got %s\nwant %s", loads[1].query, want)
	}
	// La particionada, sin ONLY; los hijos, con tope.
	if !strings.HasPrefix(loads[2].query, "SELECT id,order_id,qty FROM public.order_items WHERE") || !strings.Contains(loads[2].query, "LIMIT 9) x") {
		t.Errorf("child query: %s", loads[2].query)
	}
	// Dos claves de la misma tabla: una carga, dos condiciones.
	if strings.Count(loads[3].query, "UNION ALL") != 1 || loads[3].role != "child" {
		t.Errorf("audit: %+v", loads[3])
	}

	// Sin clave primaria: ctid (y tableoid en una particionada).
	tg.pk = ""
	if l := planSlice(tg, nil, &sliceSpec{rows: 5, related: 5}); !strings.Contains(l[0].query, "ORDER BY ctid LIMIT 5") {
		t.Errorf("no pk: %s", l[0].query)
	}
	tg.relkind = "p"
	if l := planSlice(tg, nil, &sliceSpec{rows: 5, related: 5}); !strings.Contains(l[0].query, "FROM public.orders ORDER BY tableoid, ctid LIMIT 5") {
		t.Errorf("no pk, partitioned: %s", l[0].query)
	}
	// Padre e hija a la vez.
	tg.pk, tg.relkind = "id", "r"
	both := planSlice(tg, []sliceRel{
		{dir: "parent", other: sliceTable{relkind: "r", qname: "x", plain: "x", cols: "a"}, fk: "x_id", ref: "id"},
		{dir: "child", other: sliceTable{relkind: "r", qname: "x", plain: "x", cols: "a"}, fk: "o_id", ref: "id"},
	}, &sliceSpec{rows: 5, related: 5})
	if len(both) != 2 || both[1].role != "parent+child" {
		t.Errorf("parent+child: %+v", both)
	}
}

// El guion es sh válido (sh -n) y no lleva la contraseña ni nada más que el
// marcador.
func TestSliceScriptsSintaxis(t *testing.T) {
	src, _ := parseCloneURL("postgres://reader@db.example.com:6543/shop")
	tg, rels, _ := parseSliceDiscover([]byte(sliceDiscoverOut()))
	loads := planSlice(tg, rels, &sliceSpec{rows: 10, related: 10})
	for name, s := range map[string]string{
		"discover": sliceDiscoverScript(src, "public.orders"),
		"fill":     sliceFillScript(src, loads),
	} {
		cmd := exec.Command("sh", "-n")
		cmd.Stdin = strings.NewReader(s)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: sh -n: %v\n%s", name, err, out)
		}
		if strings.Contains(s, clonePW) {
			t.Errorf("%s carries the password", name)
		}
	}
	fix := sliceFixupSQL(loads)
	for _, want := range []string{"NOT VALID", "setval", "SELECT 'sl|rows|3|' || count(*) FROM ONLY public.\"Audit Log\";", "SELECT 'sl|rows|2|' || count(*) FROM public.order_items;"} {
		if !strings.Contains(fix, want) {
			t.Errorf("fixup lacks %q", want)
		}
	}
}

func TestParseSliceFixup(t *testing.T) {
	tg, rels, _ := parseSliceDiscover([]byte(sliceDiscoverOut()))
	loads := planSlice(tg, rels, &sliceSpec{rows: 10, related: 10})
	res, err := parseSliceFixup([]byte("sl|fk|dropped|public.order_items.fk\x1b[31m\nsl|rows|0|10\nsl|rows|1|3\nsl|rows|2|0\nsl|rows|3|1\n"), loads)
	if err != nil || len(res.DroppedFKs) != 1 || strings.ContainsRune(res.DroppedFKs[0], 0x1b) {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := parseSliceFixup([]byte("sl|rows|0|10\n"), loads); err == nil {
		t.Error("missing counts accepted")
	}
	if _, err := parseSliceFixup([]byte("sl|rows|9|10\n"), loads); err == nil {
		t.Error("bad index accepted")
	}
}
