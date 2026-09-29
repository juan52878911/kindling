package dbaudit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type fake struct {
	logOut, evOut []byte
	logErr        error
}

func (f *fake) Run(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
	if args[0] == "events" {
		return f.evOut, errors.New("deadline exceeded")
	}
	return f.logOut, f.logErr
}

const sampleLog = `2026-09-28 10:00:00.100 UTC [10] u= d= h=172.16.0.1 LOG:  connection received: host=172.16.0.1 port=5555
2026-09-28 10:00:00.200 UTC [10] u=app d=app h=172.16.0.1 LOG:  connection authorized: user=app database=app
2026-09-28 10:00:05.000 UTC [10] u=app d=app h=172.16.0.1 LOG:  disconnection: session time: 0:00:04.8 user=app database=app host=172.16.0.1 port=5555
2026-09-28 10:00:06.000 UTC [11] u=app d=app h=10.0.0.9 FATAL:  password authentication failed for user "app"
2026-09-28 10:00:06.000 UTC [11] u=app d=app h=10.0.0.9 DETAIL:  Connection matched pg_hba.conf line 2: SECRETO
2026-09-28 10:00:07.000 UTC [12] u=app d=app h=[local] LOG:  statement: SELECT 'sql-secreto'
`

func setNow(t *testing.T) {
	old := now
	now = func() time.Time { return time.Date(2026, 9, 28, 10, 5, 0, 0, time.UTC) }
	t.Cleanup(func() { now = old })
}

func TestParseLog(t *testing.T) {
	es := ParseLog(sampleLog)
	var got []string
	for _, e := range es {
		got = append(got, e.Event+"/"+e.Client)
	}
	want := "connect/172.16.0.1 disconnect/172.16.0.1 auth-failed/10.0.0.9"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v", got)
	}
}

func TestRunTablaSinSQL(t *testing.T) {
	setNow(t)
	f := &fake{logOut: []byte(sampleLog), evOut: []byte(
		`{"time":"2026-09-28T10:00:00Z","type":"machine.thawed","id":"abc","name":"copia1"}` + "\n" +
			`{"time":"2026-09-28T10:00:01Z","type":"machine.frozen","name":"otra"}` + "\n")}
	var b bytes.Buffer
	if err := Run(context.Background(), f, "copia1", time.Hour, false, &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, s := range []string{"TIME", "thaw", "connect", "disconnect", "auth-failed"} {
		if !strings.Contains(out, s) {
			t.Errorf("falta %q en\n%s", s, out)
		}
	}
	for _, s := range []string{"SECRETO", "sql-secreto", "freeze"} {
		if strings.Contains(out, s) {
			t.Errorf("no debe aparecer %q", s)
		}
	}
	if strings.Index(out, "thaw") > strings.Index(out, "auth-failed") {
		t.Error("orden temporal roto")
	}
}

func TestRunSinceYJSON(t *testing.T) {
	setNow(t)
	f := &fake{logOut: []byte(sampleLog)}
	var b bytes.Buffer
	if err := Run(context.Background(), f, "copia1", 4*time.Minute+30*time.Second, true, &b); err != nil {
		t.Fatal(err)
	}
	var es []Entry
	if err := json.Unmarshal(b.Bytes(), &es); err != nil {
		t.Fatal(err)
	}
	if len(es) != 0 {
		t.Fatalf("since debía filtrar todo: %+v", es)
	}
}

func TestRunErrores(t *testing.T) {
	f := &fake{logErr: errors.New("boom")}
	if err := Run(context.Background(), f, "copia1", time.Hour, false, io.Discard); err == nil {
		t.Error("error del log debe propagarse")
	}
	if err := Run(context.Background(), f, "a b; rm", time.Hour, false, io.Discard); err == nil {
		t.Error("nombre inválido")
	}
	if err := Run(context.Background(), f, "x", 0, false, io.Discard); err == nil {
		t.Error("since 0")
	}
}
