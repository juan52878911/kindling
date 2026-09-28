package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

const (
	auditOK     = `{"ts":"2026-09-28T10:00:00.000000001Z","kind":"http","method":"GET","host":"api.example.com","path":"/v1/balance","query":true,"status":200,"creds":["KEY","ORG"],"req_bytes":0,"resp_bytes":12,"ms":41}`
	auditDenied = `{"ts":"2026-09-28T10:00:01.000000002Z","kind":"http","method":"POST","host":"api.example.com","path":"/v1/charges","status":403,"reason":"not_allowed","denied":true,"req_bytes":0,"resp_bytes":60,"ms":0,"dropped":3}`
	auditDrop   = `{"ts":"2026-09-28T10:00:02.000000003Z","kind":"dropped","req_bytes":0,"resp_bytes":0,"ms":0,"dropped":5}`
	auditBusy   = `{"ts":"2026-09-28T10:00:03.000000004Z","kind":"http","method":"GET","host":"api.example.com","path":"/","status":503,"reason":"busy","req_bytes":0,"resp_bytes":0,"ms":0}`
)

// La tabla: una fila por petición, DENIED(motivo) para las denegadas, el
// motivo para un límite, y los descartados a stderr (y no como fila).
func TestAuditWriterTabla(t *testing.T) {
	var out, errOut bytes.Buffer
	w := &auditWriter{out: &out, errOut: &errOut}
	w.header()
	for _, l := range []string{auditOK, auditDenied, auditDrop, auditBusy} {
		if _, err := w.Write([]byte(l + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("filas:\n%s", out.String())
	}
	for _, want := range []string{"TIME", "METHOD", "HOST", "PATH", "STATUS", "CREDS", "MS", "RESULT"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("la cabecera no tiene %s: %q", want, lines[0])
		}
	}
	if !strings.Contains(lines[1], "/v1/balance") || !strings.Contains(lines[1], "KEY,ORG") ||
		!strings.Contains(lines[1], " 200 ") || !strings.HasSuffix(lines[1], " ok") {
		t.Errorf("fila correcta: %q", lines[1])
	}
	if !strings.HasSuffix(lines[2], "DENIED(not_allowed)") || !strings.Contains(lines[2], " - ") {
		t.Errorf("fila denegada: %q", lines[2])
	}
	if !strings.HasSuffix(lines[3], " busy") {
		t.Errorf("fila de límite: %q", lines[3])
	}
	if errOut.String() != "dropped 3 records\ndropped 5 records\n" {
		t.Errorf("stderr: %q", errOut.String())
	}
}

// Una conexión de Postgres: rol@base en vez de ruta, sin estado HTTP y el
// método con que el proxy se autenticó ante el servidor.
func TestAuditWriterPostgres(t *testing.T) {
	var out, errOut bytes.Buffer
	w := &auditWriter{out: &out, errOut: &errOut}
	for _, l := range []string{
		`{"ts":"2026-09-28T10:00:00Z","kind":"postgres","host":"db.example.com","user":"app","database":"appdb","auth":"scram-sha-256-plus","creds":["PGPASSWORD"],"req_bytes":10,"resp_bytes":20,"ms":5}`,
		`{"ts":"2026-09-28T10:00:01Z","kind":"postgres","reason":"bad_placeholder","denied":true,"req_bytes":10,"resp_bytes":20,"ms":1}`,
		`{"ts":"2026-09-28T10:00:02Z","kind":"postgres","method":"cancel","host":"db.example.com","user":"app","creds":["PGPASSWORD"],"req_bytes":16,"resp_bytes":0,"ms":3}`,
	} {
		if _, err := w.Write([]byte(l + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], " PG ") || !strings.Contains(lines[0], "app@appdb") ||
		!strings.HasSuffix(lines[0], "ok(scram-sha-256-plus)") {
		t.Fatalf("tabla:\n%s", out.String())
	}
	if !strings.HasSuffix(lines[1], "DENIED(bad_placeholder)") || !strings.Contains(lines[2], "CANCEL") {
		t.Errorf("tabla:\n%s", out.String())
	}
}

func TestAuditWriterJSON(t *testing.T) {
	var out, errOut bytes.Buffer
	w := &auditWriter{out: &out, errOut: &errOut, json: true}
	for _, l := range []string{auditOK, auditDrop} {
		if _, err := w.Write([]byte(l + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if out.String() != auditOK+"\n"+auditDrop+"\n" || errOut.String() != "dropped 5 records\n" {
		t.Fatalf("stdout %q stderr %q", out.String(), errOut.String())
	}
}

// -f reutiliza followLogs: solo salen los registros nuevos de cada sondeo.
func TestAuditFollowSoloLoNuevo(t *testing.T) {
	src := &fakeLogs{
		pages:  []string{auditOK, auditOK + "\n" + auditDenied, auditOK + "\n" + auditDenied + "\n" + auditBusy},
		states: []bool{true, true, false},
	}
	var out, errOut bytes.Buffer
	w := &auditWriter{out: &out, errOut: &errOut}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := followLogs(ctx, w, src, []string{auditOK}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "DENIED") || !strings.Contains(lines[1], "busy") {
		t.Fatalf("seguido:\n%s", out.String())
	}
}

// Un registro con caracteres de control (de otra versión, o tocado) no llega
// al terminal tal cual.
func TestAuditWriterSinEscapes(t *testing.T) {
	var out, errOut bytes.Buffer
	w := &auditWriter{out: &out, errOut: &errOut}
	l := `{"ts":"2026-09-28T10:00:00Z","kind":"http","method":"GET","host":"a\u001b]0;x","path":"/\u001b[2J\u009b","status":200,"req_bytes":0,"resp_bytes":0,"ms":0}`
	if _, err := w.Write([]byte(l)); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\x1b\u009b") || !strings.Contains(out.String(), "/?[2J?") {
		t.Fatalf("salida %q", out.String())
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	if ts, err := parseSince("10m", now); err != nil || !ts.Equal(now.Add(-10*time.Minute)) {
		t.Fatalf("10m: %v %v", ts, err)
	}
	if ts, err := parseSince("2026-09-28T11:00:00Z", now); err != nil || !ts.Equal(now.Add(-time.Hour)) {
		t.Fatalf("RFC 3339: %v %v", ts, err)
	}
	for _, bad := range []string{"ayer", "-5m", "0s"} {
		if _, err := parseSince(bad, now); err == nil {
			t.Fatalf("%q debería fallar", bad)
		}
	}
}
