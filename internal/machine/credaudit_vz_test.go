//go:build darwin

package machine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// En macOS, CredAudit mezcla por hora el registro del proxy aparte, el del VMM
// y el que dejaban los VMM anteriores. De los dos que escribe el VMM, una
// línea que dice haber usado una credencial después de que existiera el
// proxy aparte no vale: quien usa las claves desde entonces es el proxy.
func TestCredAuditMezclaLasFuentesDeMacOS(t *testing.T) {
	m := newTestManager(t)
	m.addForTest("m1")
	mdir := m.dir("m1")
	t0 := time.Now().Add(-time.Hour).UTC()
	escribir := func(path string, recs ...credproxy.Record) {
		t.Helper()
		a := credproxy.NewAuditor(path, nil)
		for _, r := range recs {
			a.Record(r)
		}
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(mdir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Lo de un VMM anterior, de antes del proxy aparte: vale todo.
	escribir(filepath.Join(mdir, credproxy.AuditFile),
		credproxy.Record{TS: t0, Kind: credproxy.KindHTTP, Path: "/viejo", Creds: []string{"KEY"}})
	// El proxy aparte nace ahora (su directorio lo crea el daemon).
	time.Sleep(10 * time.Millisecond)
	if err := os.Mkdir(filepath.Join(mdir, credproxy.SplitDir), 0o700); err != nil {
		t.Fatal(err)
	}
	ahora := time.Now().UTC()
	escribir(m.credAuditPath("m1"),
		credproxy.Record{TS: ahora.Add(time.Second), Kind: credproxy.KindHTTP, Path: "/proxy", Creds: []string{"KEY"}},
		credproxy.Record{TS: ahora.Add(3 * time.Second), Kind: credproxy.KindHTTP, Path: "/proxy2", Creds: []string{"KEY"}})
	escribir(filepath.Join(mdir, credproxy.AuditFileVMM),
		credproxy.Record{TS: ahora.Add(2 * time.Second), Kind: credproxy.KindLink, Host: "db.graph:5432"},
		// Un VMM tomado que se inventa un uso de la clave: no sale.
		credproxy.Record{TS: ahora.Add(4 * time.Second), Kind: credproxy.KindHTTP, Path: "/falso", Creds: []string{"KEY"}})
	escribir(filepath.Join(mdir, credproxy.AuditFile),
		credproxy.Record{TS: ahora.Add(5 * time.Second), Kind: credproxy.KindHTTP, Path: "/falso-viejo", Creds: []string{"KEY"}},
		credproxy.Record{TS: ahora.Add(6 * time.Second), Kind: credproxy.KindHTTP, Path: "/negado", Denied: true, Reason: "no_credential"})

	got, err := m.CredAudit("m1", api.CredAuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var orden []string
	for _, r := range got {
		orden = append(orden, r.Path+r.Host)
	}
	want := []string{"/viejo", "/proxy", "db.graph:5432", "/proxy2", "/negado"}
	if len(orden) != len(want) {
		t.Fatalf("got %q, want %q", orden, want)
	}
	for i := range want {
		if orden[i] != want[i] {
			t.Fatalf("got %q, want %q", orden, want)
		}
	}
	// Tail se aplica a la mezcla.
	got, _ = m.CredAudit("m1", api.CredAuditQuery{Tail: 2})
	if len(got) != 2 || got[0].Path != "/proxy2" || got[1].Path != "/negado" {
		t.Fatalf("tail 2: %+v", got)
	}
}
