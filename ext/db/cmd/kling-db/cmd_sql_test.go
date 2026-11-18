package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/pgmini"
)

func TestPrintTable(t *testing.T) {
	v1, v2 := "1", "a\tb"
	var b bytes.Buffer
	printTable(&b, pgmini.Result{Command: "SELECT 2", Columns: []string{"id", "name"}, Rows: [][]*string{{&v1, &v2}, {&v1, nil}}})
	got := b.String()
	if !strings.Contains(got, "id  name\n") || !strings.Contains(got, `a\tb`) || !strings.HasSuffix(got, "(2 rows)\n") {
		t.Fatalf("table:\n%s", got)
	}
	b.Reset()
	printTable(&b, pgmini.Result{Command: "CREATE TABLE"})
	if b.String() != "CREATE TABLE\n" {
		t.Fatalf("command tag: %q", b.String())
	}
}

func TestSQLSoloPostgres(t *testing.T) {
	ta := newTestApp(t)
	mc, err := ta.up(ctx, "pg", "c1", 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	ta.f.machines[mc.ID].Labels[labelEngine] = engineRedis
	err = ta.sql(ctx, "c1", "local", "", "select 1", false, defaultSQLMaxRows, time.Second)
	if err == nil || !strings.Contains(err.Error(), "only speaks Postgres") {
		t.Fatalf("error %v", err)
	}
}

func TestSQLCopiaQueNoExiste(t *testing.T) {
	ta := newTestApp(t)
	err := ta.sql(ctx, "nope", "local", "", "select 1", false, defaultSQLMaxRows, time.Second)
	if err == nil || !strings.Contains(err.Error(), "kling db ls") {
		t.Fatalf("error %v", err)
	}
}

// execFalso apunta los topes que recibe y devuelve un resultado truncado.
type execFalso struct{ lim pgmini.Limits }

func (e *execFalso) ExecLimited(_ context.Context, _ string, lim pgmini.Limits) ([]pgmini.Result, error) {
	e.lim = lim
	v := "1"
	return []pgmini.Result{{Command: "SELECT 50000", Columns: []string{"n"}, Rows: [][]*string{{&v}}, Truncated: true}}, nil
}

// kling db sql pasa siempre los topes a la conexión y dice que truncó, en
// tabla y en -json.
func TestSQLTopesYTruncado(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		var out bytes.Buffer
		e := &execFalso{}
		a := &app{stdout: &out}
		if err := a.printSQL(context.Background(), e, "select", asJSON, 7); err != nil {
			t.Fatal(err)
		}
		if e.lim.MaxRows != 7 || e.lim.MaxBytes != sqlMaxBytes {
			t.Fatalf("limits = %+v", e.lim)
		}
		want := "truncated: the server sent SELECT 50000"
		if asJSON {
			want = `"truncated": true`
		}
		if !strings.Contains(out.String(), want) {
			t.Fatalf("json=%v: output lacks %q:\n%s", asJSON, want, out.String())
		}
	}
}

// Celdas, columnas y etiqueta vienen de la base: ningún carácter de control
// llega a la terminal.
func TestPrintTableSinControl(t *testing.T) {
	v := "a\x1b]0;x\x07b\rc"
	var b bytes.Buffer
	printTable(&b, pgmini.Result{Command: "SELECT 1\x1b[2J", Columns: []string{"c\x1b[31m"}, Rows: [][]*string{{&v}}, Truncated: true})
	got := b.String()
	for _, r := range got {
		if r != '\n' && r < 0x20 || r == 0x7f {
			t.Fatalf("control character %q in:\n%q", r, got)
		}
	}
	if !strings.Contains(got, `b\rc`) {
		t.Fatalf("carriage return not escaped: %q", got)
	}
}
