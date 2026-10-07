package machine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// maquinaConVMMSinProceso es maquinaConVMM sin el proceso de /proc: lo que
// basta para hablar con el VMM falso por su API en cualquier sistema.
func maquinaConVMMSinProceso(t *testing.T, m *Manager, id string) (*api.Machine, *fcFalso) {
	t.Helper()
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	falso := nuevoFcFalso(t)
	mc := m.addForTest(id)
	m.mu.Lock()
	m.socket[id] = falso.Sock
	m.mu.Unlock()
	return mc, falso
}

// pausarPorAlmacen filtraba por mc.Transition, que en byID no se rellena
// nunca (vive en m.transicion): una máquina a medio congelar se pausaba por
// debajo del volcado. Ahora se salta, y la de al lado sí se pausa.
func TestPausarPorAlmacenSaltaLaQueEstaEnTransicion(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	f := &almacenFalso{libre: 1 << 20, montado: true}
	m.alm = nuevoAlmacenFalso(t, m.root, f)
	m.alm.montado = true
	enTransicion, libre := "cccc000000000001", "cccc000000000002"
	falsos := map[string]*fcFalso{}
	for _, id := range []string{enTransicion, libre} {
		_, falso := maquinaConVMMSinProceso(t, m, id)
		falsos[id] = falso
		if err := os.Symlink(filepath.Join(m.alm.dirInstancia(id), "overlay.ext4"), filepath.Join(m.dir(id), "overlay.ext4")); err != nil {
			t.Fatal(err)
		}
	}
	quitar := m.marcarTransicion(enTransicion, api.TransitionFreezing)
	defer quitar()

	hechas := m.pausarPorAlmacen(context.Background())
	if len(hechas) != 1 || hechas[0] != libre {
		t.Fatalf("pausadas = %v; quería solo %s", hechas, libre)
	}
	if ll := falsos[enTransicion].todas(); len(ll) != 0 {
		t.Fatalf("a la que se congelaba se le pidió %v: se pausó por debajo del volcado", ll)
	}
}
