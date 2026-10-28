package machine

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// A3-bis: con `daemon.vmm: hv` puesto a mano sobre una raíz con una máquina de
// kling-vz viva, el volcado lo hace kling-vz. El sello y el dorado llevan el
// VMM de la máquina, no el del daemon: si no, el thaw lo daba por bueno y lo
// cargaba en kling-hv.
func TestFreezeSellaConElVMMDeLaMaquina(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	t.Setenv("KLING_MIN_FREE_DISK_MIB", "0")
	m.FijarOrigen("v0.20.0", "kling-hv 0.20.0")
	id := newID()
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	falso := nuevoFcFalso(t)
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.MemMiB = 1
	mc.VMM = "kling-vz 0.19.0"
	m.socket[id] = falso.Sock
	m.mu.Unlock()
	falso.enGancho(func(metodo, ruta string) {
		if metodo == http.MethodPut && ruta == "/snapshot/create" {
			_ = os.WriteFile(filepath.Join(dir, "snap.file"), []byte("snap"), 0o644)
			escribirPaginas(t, filepath.Join(dir, "mem.file"), paginaDe('A'))
		}
	})
	if _, err := m.Freeze(context.Background(), id); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if got := vmmDelVolcado(dir); got != "kling-vz 0.19.0" {
		t.Fatalf("sello = %q, quería el VMM de la máquina", got)
	}
	// Y el dorado que se haga de ella.
	s := &api.Snapshot{}
	m.grabarOrigen(s, &api.Machine{VMM: "kling-vz 0.19.0"})
	if s.VMM != "kling-vz 0.19.0" || s.KlingVersion != "0.20.0" {
		t.Fatalf("dorado = %q / %q", s.VMM, s.KlingVersion)
	}
	// Una máquina de un daemon anterior (sin VMM) cae al del daemon.
	m.grabarOrigen(s, &api.Machine{})
	if s.VMM != "kling-hv 0.20.0" {
		t.Fatalf("sin VMM en la máquina = %q", s.VMM)
	}
}
