package machine

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
)

// Un state.json de kling ≤ v0.13 llamaba "warm" a lo congelado. Ya no se
// traduce: NewManager no arranca, dice de dónde viene y cómo pasarlo por
// v0.17, y no toca el fichero. Uno v0 de v0.14-v0.17 ("frozen") sí arranca.
func TestEstadoV013SeRechaza(t *testing.T) {
	root := t.TempDir()
	viejo := []byte(`[{"id":"aa11bb22cc33dd44","state":"warm"},{"id":"bb22","state":"running"}]`)
	if err := os.WriteFile(filepath.Join(root, "state.json"), viejo, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewManager(root, "", "", events.New())
	if err == nil || !strings.Contains(err.Error(), "v0.13") || !strings.Contains(err.Error(), "kling v0.17") {
		t.Fatalf("err = %v, want the v0.13 refusal", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "state.json")); !bytes.Equal(b, viejo) {
		t.Fatal("the refusal touched state.json")
	}

	if err := comprobarVersionEstado(t.TempDir()); err != nil {
		t.Fatalf("no state.json: %v", err)
	}
	root = t.TempDir()
	os.WriteFile(filepath.Join(root, "state.json"), []byte(`[{"id":"aa11bb22cc33dd44","state":"frozen"}]`), 0o600)
	if err := comprobarVersionEstado(root); err != nil {
		t.Fatalf("a v0.17 state.json: %v", err)
	}
}
