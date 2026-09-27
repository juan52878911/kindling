package daemon

// Test de N3 (M-07/D-04): handleLogs valida `tail` en vez de tragarse en
// silencio un valor que no se pudo parsear y devolver el defecto como si nada.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

func TestHandleLogsValidaTail(t *testing.T) {
	root := t.TempDir()
	// Sembrada congelada, como TestRenewDeMaquina: el Manager no intenta
	// adoptar ni matar ningún proceso al arrancar.
	seed := []*api.Machine{{ID: "c3c3c3c3c3c3c3c3c3", Name: "svc", State: api.StateWarm}}
	b, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	bus := events.New()
	mgr, err := machine.NewManager(root, "", "", bus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	h := (&Server{mgr: mgr, root: root, bus: bus}).routes()

	dir := filepath.Join(root, "machines", "c3c3c3c3c3c3c3c3c3")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "firecracker.log"), []byte("l1\nl2\nl3\nl4\nl5\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tail := range []string{"abc", "-1"} {
		if rr := call(t, h, "GET", "/machines/svc/logs?tail="+tail, ""); rr.Code != http.StatusBadRequest {
			t.Fatalf("tail=%q: quería 400, salió %d %s", tail, rr.Code, rr.Body)
		}
	}

	if rr := call(t, h, "GET", "/machines/svc/logs?tail=2", ""); rr.Code != http.StatusOK {
		t.Fatalf("tail=2: quería 200, salió %d %s", rr.Code, rr.Body)
	} else if got := rr.Body.String(); got != "l4\nl5" {
		t.Fatalf("tail=2 = %q, quería %q", got, "l4\nl5")
	}

	// tail vacío sigue siendo válido: usa el defecto (200).
	if rr := call(t, h, "GET", "/machines/svc/logs", ""); rr.Code != http.StatusOK {
		t.Fatalf("sin tail: quería 200, salió %d %s", rr.Code, rr.Body)
	}
}
