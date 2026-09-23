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

// Los snapshots de v0.4 no se reescriben al actualizar: su catálogo y su salud
// tienen que aparecer como anotaciones, que es donde los busca kindling-mcp, y
// con la forma que kindling-mcp entiende.
func TestLiftV04(t *testing.T) {
	m := annotTestManager(t)
	writeSnapMeta(t, m, "eco", metaV04)

	s, err := m.loadSnapshot("eco")
	if err != nil {
		t.Fatal(err)
	}
	var tools struct {
		Tools      []struct{ Name string } `json:"tools"`
		CapturedAt string                  `json:"captured_at"`
	}
	if ok, err := s.Annotation("mcp.tools", &tools); !ok || err != nil || len(tools.Tools) != 1 ||
		tools.Tools[0].Name != "echo" || tools.CapturedAt == "" {
		t.Fatalf("mcp.tools mal elevado: ok=%v err=%v %+v", ok, err, tools)
	}
	var h struct{ Status, At, Error string }
	if ok, _ := s.Annotation("mcp.health", &h); !ok || h.Status != "unhealthy" || h.Error != "timeout" || h.At == "" {
		t.Fatalf("mcp.health mal elevado: %+v", h)
	}
	// Leer no escribe; la siguiente anotación deja el meta sin los campos viejos.
	if _, err := m.SetAnnotation("eco", "otra", json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(m.snapDir("eco"), "meta.json"))
	if strings.Contains(string(b), `"tools_at"`) || !strings.Contains(string(b), `"mcp.tools"`) {
		t.Fatalf("tras escribir, el meta debe quedar solo con anotaciones: %s", b)
	}
	// Un snapshot de v0.4 que nunca se sondeó no se inventa una salud.
	writeSnapMeta(t, m, "nuevo", `{"name":"nuevo","image":"x","tools":[]}`)
	s, _ = m.loadSnapshot("nuevo")
	if _, ok := s.Annotations["mcp.health"]; ok {
		t.Fatal("sin salud en v0.4 no hay mcp.health")
	}
}
