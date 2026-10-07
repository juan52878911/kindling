package machine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEsquemasEnDisco(t *testing.T) {
	root := t.TempDir()
	if e, err := EsquemasEnDisco(root); err != nil || e != (Esquemas{}) {
		t.Fatalf("empty root: %+v %v", e, err)
	}
	escribir := func(rel, contenido string) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(contenido), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	escribir("state.json", `{"schema":2,"machines":[]}`)
	escribir("snapshots/a/meta.json", `{"name":"a"}`)
	escribir("snapshots/b/meta.json", `{"schema":1,"name":"b"}`)
	escribir("machines/x/credentials.enc", "\x00\x01nonce-v0")
	escribir("store/graph/g.secrets.enc", "KLCS\x01nonce")
	e, err := EsquemasEnDisco(root)
	if err != nil {
		t.Fatal(err)
	}
	if e != (Esquemas{Estado: 2, Meta: 1, Credenciales: 1}) {
		t.Errorf("got %+v", e)
	}
	if mal := EsquemasSoportados().Incompatibles(e); len(mal) != 0 {
		t.Errorf("this binary reads its own formats: %v", mal)
	}

	// Lo que deja un kling más nuevo se nombra, fichero por fichero.
	escribir("secrets/credentials/t.enc", "KLCS\x07nonce")
	escribir("snapshots/c/meta.json", `{"schema":9}`)
	e, _ = EsquemasEnDisco(root)
	mal := EsquemasSoportados().Incompatibles(e)
	if len(mal) != 2 || !strings.Contains(mal[0], "credential") || !strings.Contains(mal[1], "meta.json") {
		t.Errorf("incompatibles: %v", mal)
	}

	// Un kling ≤ v0.17 (todo en 0) no lee el state.json v1.
	if mal := (Esquemas{}).Incompatibles(Esquemas{Estado: 1}); len(mal) != 1 || !strings.Contains(mal[0], "state.json") {
		t.Errorf("v0.17 against state v1: %v", mal)
	}
	if m := EsquemasSoportados().Migraciones(Esquemas{}); len(m) != 3 {
		t.Errorf("from v0.17 everything migrates: %v", m)
	}
}
