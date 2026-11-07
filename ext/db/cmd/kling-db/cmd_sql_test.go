package main

import (
	"bytes"
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
	err = ta.sql(ctx, "c1", "local", "", "select 1", false, time.Second)
	if err == nil || !strings.Contains(err.Error(), "only speaks Postgres") {
		t.Fatalf("error %v", err)
	}
}

func TestSQLCopiaQueNoExiste(t *testing.T) {
	ta := newTestApp(t)
	err := ta.sql(ctx, "nope", "local", "", "select 1", false, time.Second)
	if err == nil || !strings.Contains(err.Error(), "kling db ls") {
		t.Fatalf("error %v", err)
	}
}
