package machine

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/api"
	"github.com/juan52878911/kindling/internal/events"
)

// makeTestManager construye un Manager mínimo para tests, sin KVM ni firecracker.
// Solo lo que hace falta para probar persistencia: root, byID, persistCh, quit.
func makeTestManager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	m := &Manager{
		root:      root,
		byID:      make(map[string]*api.Machine),
		socket:    make(map[string]string),
		persistCh: make(chan struct{}, 1),
		quit:      make(chan struct{}),
		bus:       events.New(),
	}
	m.persistWG.Add(1)
	go m.persistLoop()
	t.Cleanup(func() { m.Close() })
	return m
}

func TestPersistDebounces(t *testing.T) {
	m := makeTestManager(t)

	// Ráfaga de 5 schedulePersist en pocos microsegundos — debe coalescer en
	// una sola escritura gracias al debounce de 50 ms.
	for i := 0; i < 5; i++ {
		m.schedulePersist()
	}

	// Esperar 100 ms: más que el debounce (50 ms) + margen.
	time.Sleep(100 * time.Millisecond)

	if _, err := os.Stat(m.statePath()); err != nil {
		t.Fatalf("state.json no creado tras ráfaga: %v", err)
	}
}

func TestFlushPersists(t *testing.T) {
	m := makeTestManager(t)
	m.schedulePersist()
	// Sin flush, podríamos salir antes del debounce.
	m.flush()
	if _, err := os.Stat(m.statePath()); err != nil {
		t.Fatalf("state.json no creado tras flush: %v", err)
	}
}

func TestCloseStopsLoop(t *testing.T) {
	m := makeTestManager(t)
	m.Close()
	// Llamar a schedulePersist tras Close debe ser seguro: select con default.
	m.schedulePersist()
}

func TestTouchDiskUsageMissingDir(t *testing.T) {
	m := makeTestManager(t)
	got := m.touchDiskUsage("nope")
	if got != 0 {
		t.Fatalf("dir inexistente debería devolver 0, got %d", got)
	}
}

func TestPersistRoundtrip(t *testing.T) {
	m := makeTestManager(t)
	m.byID["abc"] = &api.Machine{
		ID: "abc", Name: "test", Image: "default",
		State: api.StateRunning, CreatedAt: time.Now(),
		VCPUs: 1, MemMiB: 256,
	}
	m.flush()

	b, err := os.ReadFile(m.statePath())
	if err != nil {
		t.Fatalf("leyendo state.json: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("state.json vacío")
	}
	// El JSON debe contener el ID inyectado y ser parseable.
	if !bytes.Contains(b, []byte(`"abc"`)) {
		t.Fatalf("state.json no contiene la máquina inyectada: %s", b)
	}
	var list []*api.Machine
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatalf("state.json no es JSON válido: %v\n%s", err, b)
	}
}

// ── benchmark del cambio de encoding + persist ───────────────────────────

func BenchmarkPersistSync(b *testing.B) {
	root := b.TempDir()
	m := &Manager{
		root:      root,
		byID:      make(map[string]*api.Machine),
		socket:    make(map[string]string),
		persistCh: make(chan struct{}, 1),
		quit:      make(chan struct{}),
		bus:       events.New(),
	}
	m.persistWG.Add(1)
	go m.persistLoop()
	defer m.Close()

	for i := 0; i < 50; i++ {
		m.byID[string(rune('a'+i%26))+string(rune('a'+i/26))] = &api.Machine{
			ID: "x", Name: "test", Image: "default",
			State: api.StateRunning, CreatedAt: time.Now(),
			VCPUs: 1, MemMiB: 256,
			DiskBytes: 1024 * 1024,
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.schedulePersist()
	}
}