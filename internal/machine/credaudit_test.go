package machine

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// Lo que escribe el proxy (pkg/credproxy) es lo que lee el daemon: .1 antes
// que el actual, filtros por denegadas y por fecha, tail al final, y las
// líneas de descartados siempre.
func TestCredAuditLeeLoQueEscribeElProxy(t *testing.T) {
	m := newTestManager(t)
	m.addForTest("m1")
	if err := os.MkdirAll(m.dir("m1"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Sin registro todavía: vacío, no error.
	if got, err := m.CredAudit("m1", api.CredAuditQuery{}); err != nil || len(got) != 0 {
		t.Fatalf("sin registro: %v %v", got, err)
	}
	if _, err := m.CredAudit("nadie", api.CredAuditQuery{}); err == nil {
		t.Fatal("una máquina que no existe debe fallar")
	}
	var se *api.StatusError
	if _, err := m.CredAudit("nadie", api.CredAuditQuery{}); !errors.As(err, &se) || se.Code != 404 {
		t.Fatalf("una máquina que no existe debe ser 404: %v", err)
	}

	path := m.credAuditPath("m1")
	t0 := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	escribir := func(recs ...credproxy.Record) {
		a := credproxy.NewAuditor(path, nil)
		for _, r := range recs {
			a.Record(r)
		}
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// Dos generaciones rotadas: se leen todas, de la más vieja a la actual.
	escribir(credproxy.Record{TS: t0.Add(-time.Minute), Kind: credproxy.KindHTTP, Method: "GET", Host: "a.com", Path: "/viejisimo", Status: 200, Dropped: 3, Rotated: 3})
	if err := os.Rename(path, path+".2"); err != nil {
		t.Fatal(err)
	}
	escribir(credproxy.Record{TS: t0, Kind: credproxy.KindHTTP, Method: "GET", Host: "a.com", Path: "/viejo", Status: 200})
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	escribir(
		credproxy.Record{TS: t0.Add(time.Minute), Kind: credproxy.KindHTTP, Method: "GET", Host: "a.com", Path: "/x",
			Status: 403, Reason: credproxy.ReasonNotAllowed, Denied: true},
		credproxy.Record{TS: t0.Add(2 * time.Minute), Kind: credproxy.KindHTTP, Method: "POST", Host: "a.com", Path: "/y",
			Status: 200, Creds: []string{"KEY"}, ReqBytes: 10, RespBytes: 20, MS: 5},
		credproxy.Record{TS: t0.Add(3 * time.Minute), Kind: credproxy.KindDropped, Dropped: 7},
	)
	// Una línea rota a mitad no para la lectura.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"ts":"2026-09-28T10:0`)
	f.Close()

	todo, err := m.CredAudit("m1", api.CredAuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(todo) != 5 || todo[0].Path != "/viejisimo" || todo[0].Rotated != 3 {
		t.Fatalf("la generación .2 no se leyó: %+v", todo)
	}
	todo = todo[1:]
	if len(todo) != 4 || todo[0].Path != "/viejo" || todo[2].Creds[0] != "KEY" || todo[2].RespBytes != 20 || todo[3].Dropped != 7 {
		t.Fatalf("todo: %+v", todo)
	}
	den, _ := m.CredAudit("m1", api.CredAuditQuery{Denied: true})
	if len(den) != 2 || den[0].Path != "/x" || den[1].Kind != credproxy.KindDropped {
		t.Fatalf("denegadas: %+v", den)
	}
	desde, _ := m.CredAudit("m1", api.CredAuditQuery{Since: t0.Add(90 * time.Second)})
	if len(desde) != 2 || desde[0].Path != "/y" {
		t.Fatalf("desde: %+v", desde)
	}
	cola, _ := m.CredAudit("m1", api.CredAuditQuery{Tail: 1})
	if len(cola) != 1 || cola[0].Kind != credproxy.KindDropped {
		t.Fatalf("tail 1: %+v", cola)
	}
}

// Un enlace plantado en lugar del registro no se sigue.
func TestCredAuditNoSigueEnlaces(t *testing.T) {
	m := newTestManager(t)
	m.addForTest("m1")
	if err := os.MkdirAll(m.dir("m1"), 0o755); err != nil {
		t.Fatal(err)
	}
	ajeno := m.dir("m1") + "/ajeno"
	if err := os.WriteFile(ajeno, []byte(`{"ts":"2026-09-28T10:00:00Z","kind":"http","path":"/secreto"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ajeno, m.credAuditPath("m1")); err != nil {
		t.Fatal(err)
	}
	got, err := m.CredAudit("m1", api.CredAuditQuery{})
	if err == nil || len(got) != 0 {
		t.Fatalf("se leyó a través de un enlace: %+v %v", got, err)
	}
	if strings.Contains(err.Error(), "/secreto") {
		t.Fatalf("el error filtra el contenido: %v", err)
	}
}
