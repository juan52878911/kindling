package machine

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// overlayDe crea (o cambia de tamaño) el overlay de id, disperso.
func overlayDe(t *testing.T, m *Manager, id string, tam int64) {
	t.Helper()
	d := filepath.Join(m.root, "machines", id)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(d, "overlay.ext4"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(tam); err != nil {
		t.Fatal(err)
	}
}

func estadoVigia(m *Manager, id string) api.State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byID[id].State
}

// Sin almacén no hay cuota del núcleo sobre el overlay: un VMM que lo alarga
// más allá de lo que su invitado puede direccionar se para (antes llenaba el
// disco del host). Lo normal, y lo que ya era mayor al empezar a vigilarlo, no.
func TestVigiaOverlaysParaLaQueCrece(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	ahora := time.Now()
	for _, id := range []string{"normal", "inflada", "grande", "disco2g"} {
		mc := m.addForTest(id)
		m.mu.Lock()
		mc.StartedAt = &ahora
		if id == "disco2g" {
			mc.DiskMiB = 2048
		}
		m.mu.Unlock()
	}
	logico := int64(defaultOverlayMiB) << 20
	overlayDe(t, m, "normal", logico)
	overlayDe(t, m, "inflada", logico)
	overlayDe(t, m, "grande", 4*logico) // ya venía así: se vigila desde ahí
	overlayDe(t, m, "disco2g", 2048<<20)

	topes := map[string]topeOverlay{}
	if p := m.vueltaVigiaOverlays(topes); len(p) != 0 {
		t.Fatalf("primera vuelta paró %v; no quería ninguna", p)
	}

	overlayDe(t, m, "inflada", 64<<30) // ftruncate de un VMM comprometido
	overlayDe(t, m, "normal", logico+1<<20)
	if p := m.vueltaVigiaOverlays(topes); !slices.Equal(p, []string{"inflada"}) {
		t.Fatalf("paró %v; quería [inflada]", p)
	}
	if st := estadoVigia(m, "inflada"); st == api.StateRunning {
		t.Fatalf("la inflada sigue %s", st)
	}
	for _, id := range []string{"normal", "grande", "disco2g"} {
		if st := estadoVigia(m, id); st != api.StateRunning {
			t.Errorf("%s quedó %s; quería running", id, st)
		}
	}

	overlayDe(t, m, "grande", 8*logico)
	if p := m.vueltaVigiaOverlays(topes); !slices.Equal(p, []string{"grande"}) {
		t.Fatalf("la que ya era grande creció más y se paró %v; quería [grande]", p)
	}
}
