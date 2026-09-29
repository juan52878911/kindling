package dbaudit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Registro de server_audit con server_audit_events=CONNECT, y líneas que no
// deben interpretarse (otra operación, comas de más, basura).
const sampleMyAudit = `20260928 10:00:00,copia,app,172.16.0.1,5,0,CONNECT,appdb,,0
20260928 10:00:04,copia,app,172.16.0.1,5,0,DISCONNECT,appdb,,0
20260928 10:00:06,copia,app,10.0.0.9,6,0,FAILED_CONNECT,appdb,,1045
20260928 10:00:07,copia,root,localhost,7,0,CONNECT,,,1045
20260928 10:00:08,copia,app,172.16.0.1,8,3,QUERY,appdb,'SELECT sql-secreto',0
20260928 10:00:09,copia,a,b,SECRETO,172.16.0.1,9,0,CONNECT,appdb,,0
no es una línea
`

func TestParseMyAudit(t *testing.T) {
	es := ParseMyAudit(sampleMyAudit)
	var got []string
	for _, e := range es {
		got = append(got, e.Time.Format("15:04:05")+" "+e.Event+" "+e.User+" "+e.DB+" "+e.Client)
	}
	want := []string{
		"10:00:00 connect app appdb 172.16.0.1",
		"10:00:04 disconnect app appdb 172.16.0.1",
		"10:00:06 auth-failed app appdb 10.0.0.9",
		"10:00:07 auth-failed root  localhost",
		"10:00:09 connect ? ? ?",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, e := range es {
		b, _ := json.Marshal(e)
		if strings.Contains(string(b), "SECRETO") || strings.Contains(string(b), "sql-secreto") {
			t.Fatalf("texto del registro en la salida: %s", b)
		}
	}
}

func TestRunEngineMySQL(t *testing.T) {
	setNow(t)
	f := &fake{logOut: []byte(sampleMyAudit)}
	var out bytes.Buffer
	if err := RunEngine(context.Background(), f, "copia", "mysql", 24*time.Hour, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "auth-failed") || strings.Contains(out.String(), "note:") {
		t.Fatalf("salida:\n%s", out.String())
	}

	// Sin registro: una nota y los eventos del daemon; con -json, sin nota.
	f = &fake{logErr: errors.New("tail: no such file")}
	out.Reset()
	if err := RunEngine(context.Background(), f, "copia", "mysql", 24*time.Hour, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "note: no connection log") {
		t.Fatalf("salida:\n%s", out.String())
	}
	out.Reset()
	if err := RunEngine(context.Background(), f, "copia", "mysql", 24*time.Hour, true, &out); err != nil {
		t.Fatal(err)
	}
	var v []Entry
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("-json no es JSON: %v\n%s", err, out.String())
	}
	if err := RunEngine(context.Background(), f, "copia", "oracle", 24*time.Hour, true, &out); err == nil {
		t.Fatal("motor desconocido aceptado")
	}
}
