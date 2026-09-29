package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// observeFake contesta, sobre el kling falso de siempre, el SQL de observe
// (como superusuario) y el tail del log.
type observeFake struct {
	*fakeKling
	sqls    []string
	log     string
	tailCmd []string
}

func (o *observeFake) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	var in []byte
	if stdin != nil {
		in, _ = io.ReadAll(stdin)
	}
	if args[0] == "exec" {
		switch {
		case strings.Contains(string(in), "ALTER DATABASE"):
			o.sqls = append(o.sqls, string(in))
			return []byte("ok\n"), nil
		case indexOf(args, "tail") > 0:
			o.tailCmd = args
			return []byte(o.log), nil
		}
	}
	return o.fakeKling.Run(ctx, strings.NewReader(string(in)), args...)
}

func newObserveApp(t *testing.T) (*testApp, *observeFake) {
	t.Helper()
	ta := newTestApp(t)
	if _, err := ta.up(ctx, "pg", "c1", 0, "local"); err != nil {
		t.Fatal(err)
	}
	of := &observeFake{fakeKling: ta.f}
	ta.app.k = of
	ta.out.Reset()
	return ta, of
}

// Un log como el de la copia: log_line_prefix de db-golden.sh, sentencias de
// una línea y de varias, protocolo extendido (parse, bind y execute), otra
// base, los DETAIL y una conexión.
const observeLog = `ial line cortada por tail -c
2026-09-29 10:00:00.001 UTC [101] u=app d=appdb h=172.16.0.1 LOG:  connection authorized: user=app database=appdb
2026-09-29 10:00:00.010 UTC [101] u=app d=appdb h=172.16.0.1 LOG:  duration: 1.500 ms  statement: SELECT * FROM orders WHERE id = 42
2026-09-29 10:00:00.020 UTC [101] u=app d=appdb h=172.16.0.1 LOG:  duration: 2.500 ms  statement: SELECT * FROM orders WHERE id = 7
2026-09-29 10:00:00.030 UTC [101] u=app d=appdb h=172.16.0.1 LOG:  duration: 0.100 ms  statement: SELECT 'orders are secret', 'tarjeta 4111-1111'
2026-09-29 10:00:00.040 UTC [102] u=app d=appdb h=172.16.0.1 LOG:  duration: 0.300 ms  parse <unnamed>: UPDATE public.orders SET total = $1 WHERE id = $2
2026-09-29 10:00:00.041 UTC [102] u=app d=appdb h=172.16.0.1 LOG:  duration: 0.050 ms  bind <unnamed>: UPDATE public.orders SET total = $1 WHERE id = $2
2026-09-29 10:00:00.042 UTC [102] u=app d=appdb h=172.16.0.1 LOG:  duration: 4.000 ms  execute <unnamed>: UPDATE public.orders SET total = $1 WHERE id = $2
2026-09-29 10:00:00.043 UTC [102] u=app d=appdb h=172.16.0.1 DETAIL:  Parameters: $1 = '99.90', $2 = '42'
2026-09-29 10:00:00.050 UTC [103] u=app d=appdb h=172.16.0.1 LOG:  duration: 10.000 ms  statement: SELECT o.id,
	u.email -- el correo
	FROM orders o JOIN users u ON u.id = o.user_id
	WHERE o.id IN (1, 2, 3) AND u.email = E'a\'b@x.com' AND o.note = $tag$orders$tag$
2026-09-29 10:00:00.060 UTC [104] u=app d=other h=172.16.0.1 LOG:  duration: 50.000 ms  statement: SELECT * FROM orders
2026-09-29 10:00:00.070 UTC [105] u=app d=appdb h=172.16.0.1 LOG:  duration: 0.200 ms  statement: SELECT * FROM archive.orders
2026-09-29 10:00:00.080 UTC [105] u=app d=appdb h=172.16.0.1 LOG:  duration: 0.200 ms  statement: SELECT * FROM orders_history
2026-09-29 10:00:00.090 UTC [105] u=app d=appdb h=172.16.0.1 LOG:  duration: 0.700 ms  statement: DELETE FROM "orders" WHERE id = 1
`

func TestAnalyzeObserveLog(t *testing.T) {
	rep := analyzeObserveLog(observeLog, "appdb", "public", "orders", 20)
	// appdb: 2 + 1 + 1 (execute) + 1 (multilínea) + 3 = 8; parse y bind no cuentan.
	if rep.Logged != 8 {
		t.Errorf("logged = %d", rep.Logged)
	}
	// orders: los dos SELECT por id, el UPDATE, el JOIN y el DELETE.
	if rep.Matching != 5 || rep.Distinct != 4 {
		t.Fatalf("matching %d distinct %d: %+v", rep.Matching, rep.Distinct, rep.Statements)
	}
	want := map[string]observeStmt{
		"SELECT * FROM orders WHERE id = ?":                       {Calls: 2, TotalMS: 4, MaxMS: 2.5, MeanMS: 2},
		"UPDATE public.orders SET total = $1 WHERE id = $2":       {Calls: 1, TotalMS: 4, MaxMS: 4, MeanMS: 4},
		`DELETE FROM "orders" WHERE id = ?`:                       {Calls: 1, TotalMS: 0.7, MaxMS: 0.7, MeanMS: 0.7},
		"SELECT o.id, u.email FROM orders o JOIN users u ON u.id": {},
	}
	for _, s := range rep.Statements {
		for k, w := range want {
			if !strings.HasPrefix(s.Statement, k) {
				continue
			}
			if w.Calls != 0 && (s.Calls != w.Calls || s.TotalMS != w.TotalMS || s.MaxMS != w.MaxMS || s.MeanMS != w.MeanMS) {
				t.Errorf("%q: %+v", k, s)
			}
			delete(want, k)
		}
		// Ni un literal: ni el valor, ni el correo, ni la tarjeta.
		for _, bad := range []string{"42", "99.90", "a'b", "b@x.com", "4111", "secret", "$tag$", "el correo", "IN (1"} {
			if strings.Contains(s.Statement, bad) {
				t.Errorf("literal %q in %q", bad, s.Statement)
			}
		}
	}
	if len(want) != 0 {
		t.Errorf("missing statements %v in %+v", want, rep.Statements)
	}
	// Orden: más tiempo total primero (el JOIN, 10 ms).
	if !strings.HasPrefix(rep.Statements[0].Statement, "SELECT o.id") {
		t.Errorf("order: %+v", rep.Statements)
	}
	if !strings.Contains(rep.Statements[0].Statement, "IN (?, ...)") {
		t.Errorf("list not collapsed: %q", rep.Statements[0].Statement)
	}

	// Sin esquema en la tabla pedida: archive.orders también cuenta.
	if r := analyzeObserveLog(observeLog, "appdb", "", "orders", 20); r.Matching != 6 {
		t.Errorf("without schema: %d", r.Matching)
	}
	// El límite.
	if r := analyzeObserveLog(observeLog, "appdb", "public", "orders", 1); len(r.Statements) != 1 || r.Distinct != 4 {
		t.Errorf("limit: %+v", r)
	}
}

func TestNormalizeSQL(t *testing.T) {
	for in, want := range map[string]string{
		"SELECT 1": "SELECT ?",
		"select * from t where a = 'x''y' and b=2": "select * from t where a = ? and b=?",
		"SELECT t1.c2 FROM t1":                     "SELECT t1.c2 FROM t1",
		"SELECT $1, $2::int":                       "SELECT $1, $2::int",
		"SELECT $$a;b$$, $x$ 'q' $x$":              "SELECT ?, ...",
		"SELECT /* a /* b */ c */ 1.5e-3;":         "SELECT ?",
		`SELECT "we""ird" FROM "T 1"`:              `SELECT "we""ird" FROM "T 1"`,
		"SELECT e'\\'' , 1":                        "SELECT ?, ...",
		"INSERT INTO t VALUES (1, 2, 3)":           "INSERT INTO t VALUES (?, ...)",
		"SELECT 'unterminated":                     "SELECT ?",
		"SELECT \x1b[31m 1":                        "SELECT [?m ?",
		"SELECT 'ñandú' AS año":                    "SELECT ? AS año",
	} {
		if got := normalizeSQL(in); got != want {
			t.Errorf("normalizeSQL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTouchesTable(t *testing.T) {
	for _, c := range []struct {
		sql, schema, table string
		want               bool
	}{
		{"SELECT * FROM orders", "public", "orders", true},
		{"SELECT * FROM ORDERS", "public", "orders", true},
		{`SELECT * FROM "ORDERS"`, "public", "orders", false},
		{`SELECT * FROM "Audit Log"`, "public", "Audit Log", true},
		{"SELECT * FROM public.orders", "public", "orders", true},
		{"SELECT * FROM x.orders", "public", "orders", false},
		{"SELECT * FROM x.orders", "", "orders", true},
		{"SELECT * FROM orders_old", "public", "orders", false},
		{"SELECT ?", "public", "orders", false},
	} {
		if got := touchesTable(c.sql, c.schema, c.table); got != c.want {
			t.Errorf("touchesTable(%q, %q, %q) = %v", c.sql, c.schema, c.table, got)
		}
	}
}

func TestObserveOnOffYReport(t *testing.T) {
	ta, of := newObserveApp(t)
	if err := ta.observeSet(ctx, "c1", "local", true); err != nil {
		t.Fatal(err)
	}
	if len(of.sqls) != 1 || !strings.Contains(of.sqls[0], "ALTER DATABASE appdb SET log_min_duration_statement = 0") ||
		!strings.Contains(of.sqls[0], "log_parameter_max_length = 0") ||
		!strings.Contains(of.sqls[0], "ALTER ROLE postgres IN DATABASE appdb SET log_min_duration_statement = -1") {
		t.Fatalf("enable SQL: %q", of.sqls)
	}
	if !strings.Contains(ta.out.String(), "pg_restore_relation_stats") || !strings.Contains(ta.out.String(), "NEW connections") {
		t.Errorf("enable output:\n%s", ta.out)
	}
	if err := ta.observeSet(ctx, "c1", "local", false); err != nil {
		t.Fatal(err)
	}
	if len(of.sqls) != 2 || !strings.Contains(of.sqls[1], "RESET log_min_duration_statement") {
		t.Fatalf("disable SQL: %q", of.sqls)
	}
	// Otro dueño: no.
	if err := ta.observeSet(ctx, "c1", "otro", true); err == nil {
		t.Fatal("another owner could observe")
	}

	// Sin -table ni slice.json del golden: se pide -table.
	of.log = observeLog
	if _, err := ta.observeReport(ctx, "c1", "local", "", 20); err == nil || !strings.Contains(err.Error(), "-table") {
		t.Fatalf("err = %v", err)
	}
	// Con el slice.json del golden (pg), la tabla sale de ahí.
	if err := writeSliceMeta("pg", "reader@db:5432/shop", &sliceResult{Table: "public.orders", RowsLimit: 10, RelatedMax: 10}); err != nil {
		t.Fatal(err)
	}
	rep, err := ta.observeReport(ctx, "c1", "local", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Table != "public.orders" || rep.Matching != 5 || rep.Copy != "c1" {
		t.Fatalf("report: %+v", rep)
	}
	if strings.Join(of.tailCmd[indexOf(of.tailCmd, "--")+1:], " ") != "tail -c 33554432 "+pgLogFile {
		t.Errorf("tail: %v", of.tailCmd)
	}
	var buf bytes.Buffer
	if err := writeObserveReport(&buf, rep, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"5 statement(s) touching public.orders", "CALLS", "warning:", "PostgreSQL 18"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, buf.String())
		}
	}
	buf.Reset()
	if err := writeObserveReport(&buf, rep, true); err != nil {
		t.Fatal(err)
	}
	var js observeReport
	if err := json.Unmarshal(buf.Bytes(), &js); err != nil || js.Matching != 5 {
		t.Fatalf("json: %v %s", err, buf.String())
	}
	// -table manda sobre el slice.json; una tabla con control, no.
	if rep, err := ta.observeReport(ctx, "c1", "local", "users", 20); err != nil || rep.Matching != 1 {
		t.Fatalf("users: %+v %v", rep, err)
	}
	if _, err := ta.observeReport(ctx, "c1", "local", "a\x1bb", 20); err == nil {
		t.Fatal("control characters accepted")
	}
	// Log vacío: lo dice.
	of.log = ""
	rep, _ = ta.observeReport(ctx, "c1", "local", "orders", 20)
	buf.Reset()
	writeObserveReport(&buf, rep, false)
	if !strings.Contains(buf.String(), "is the observation on?") {
		t.Errorf("empty log:\n%s", buf.String())
	}
}
