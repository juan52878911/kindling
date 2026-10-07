package machine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

func annotTestManager(t *testing.T) *Manager {
	t.Helper()
	m := newTestManager(t)
	m.bus = events.New()
	m.priv = &Privileges{}
	return m
}

func writeSnapMeta(t *testing.T, m *Manager, name, meta string) {
	t.Helper()
	dir := m.snapDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Un meta.json tal como lo escribía v0.4: catálogo y salud en campos propios.
const metaV04 = `{
  "name": "eco", "image": "eco", "created_at": "2026-08-10T10:00:00Z",
  "vcpus": 1, "mem_mib": 256, "labels": {"service": "eco"},
  "tools": [{"name": "echo", "description": "repite", "inputSchema": {"type": "object"}}],
  "tools_at": "2026-08-10T10:00:05Z",
  "health": "unhealthy", "health_at": "2026-08-11T09:00:00Z", "health_err": "timeout"
}`

func TestSetAnnotationValida(t *testing.T) {
	m := annotTestManager(t)
	writeSnapMeta(t, m, "svc", `{"name":"svc","image":"svc"}`)

	casos := []struct {
		key, val, quiere string
	}{
		{"Mayus", `1`, "invalid annotation key"},
		{"../fuera", `1`, "invalid annotation key"},
		{"ok", `{no es json`, "not valid JSON"},
		{"ok", `"` + strings.Repeat("x", api.MaxValueBytes) + `"`, "the limit"},
	}
	for _, c := range casos {
		_, err := m.SetAnnotation("svc", c.key, json.RawMessage(c.val))
		if err == nil || !strings.Contains(err.Error(), c.quiere) {
			t.Errorf("SetAnnotation(%q): quería %q, salió %v", c.key, c.quiere, err)
		}
	}
	if _, err := m.SetAnnotation("no-existe", "k", json.RawMessage(`1`)); err == nil {
		t.Error("anotar un snapshot inexistente debe fallar")
	}

	for i := 0; i < api.MaxAnnotations; i++ {
		if _, err := m.SetAnnotation("svc", fmt.Sprintf("k%d", i), json.RawMessage(`1`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.SetAnnotation("svc", "una-mas", json.RawMessage(`1`)); err == nil {
		t.Fatal("pasar del máximo de anotaciones debe fallar")
	}
	// Reescribir una existente sí se permite aunque esté al máximo.
	if _, err := m.SetAnnotation("svc", "k0", json.RawMessage(`2`)); err != nil {
		t.Fatalf("reescribir una anotación existente: %v", err)
	}
}

// Dos escritores a la vez sobre el mismo meta no pueden perder anotaciones: es
// el caso del gateway marcando salud mientras el CLI guarda el catálogo.
func TestAnotacionesConcurrentesNoSePierden(t *testing.T) {
	m := annotTestManager(t)
	writeSnapMeta(t, m, "svc", `{"name":"svc","image":"svc"}`)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := m.SetAnnotation("svc", fmt.Sprintf("k%d", i), json.RawMessage(`true`)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	s, err := m.loadSnapshot("svc")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Annotations) != 20 {
		t.Fatalf("se perdieron anotaciones: quedan %d de 20", len(s.Annotations))
	}
}

// Un meta.json de v0.4 (catálogo y salud en campos propios) ya no se eleva a
// anotaciones: se rechaza diciendo de dónde viene y qué hacer, sin tocarlo.
// Uno v0 sin esos campos (v0.5 a v0.17) se sigue leyendo.
func TestMetaV04SeRechaza(t *testing.T) {
	m := annotTestManager(t)
	writeSnapMeta(t, m, "eco", metaV04)

	_, err := m.loadSnapshot("eco")
	if err == nil || !strings.Contains(err.Error(), "kling v0.4") || !strings.Contains(err.Error(), "kling save") {
		t.Fatalf("err = %v, want the v0.4 refusal", err)
	}
	if _, err := m.SetAnnotation("eco", "otra", json.RawMessage(`1`)); err == nil {
		t.Fatal("annotating a v0.4 meta must fail")
	}
	if b, _ := os.ReadFile(filepath.Join(m.snapDir("eco"), "meta.json")); string(b) != metaV04 {
		t.Fatalf("the refusal rewrote the meta: %s", b)
	}
	for _, s := range m.Snapshots() {
		if s.Name == "eco" {
			t.Fatal("a v0.4 golden must not be listed as usable")
		}
	}

	writeSnapMeta(t, m, "v017", `{"name":"v017","image":"x","annotations":{"mcp.tools":{"tools":[]}}}`)
	if _, err := m.loadSnapshot("v017"); err != nil {
		t.Fatalf("a v0.17 meta (v0, no v0.4 fields): %v", err)
	}
}
