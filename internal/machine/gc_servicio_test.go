package machine

import (
	"context"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// Con el disco lleno, el GC solo borra congeladas de un servicio. Una copia de
// kling db congelada al cambiar de rama (o una de fork, o una de run -from)
// tiene en su disco lo que escribió desde su dorado: el dorado no la recrea, y
// borrarla era perder la rama (visto en el laboratorio con el disco al 88 %).
func TestGCDiskSoloBorraLasDeUnServicio(t *testing.T) {
	t.Setenv("KLING_GC_DISK_HIGH", "1") // cualquier disco está "lleno"
	t.Setenv("KLING_GC_DISK_TARGET", "0")
	m := newTestManager(t)
	m.bus = events.New()
	if m.diskUsedPct() < 1 {
		t.Skip("no se puede medir el disco del temporal")
	}
	escribirSnapshot(t, m, "dorado", api.Snapshot{})
	if _, err := m.loadSnapshot("dorado"); err != nil {
		t.Skipf("el snapshot de prueba no carga: %v", err)
	}
	congelada := time.Now().Add(-time.Hour)
	nueva := func(name string, labels map[string]string) string {
		id := newID()
		m.byID[id] = &api.Machine{ID: id, Name: name, State: api.StateWarm,
			From: "dorado", FrozenAt: &congelada, Labels: labels}
		return id
	}
	rama := nueva("db-rama", map[string]string{api.LabelDBGolden: "dorado", api.LabelDBState: "ready"})
	suelta := nueva("run-from", nil)
	servicio := nueva("svc", map[string]string{api.LabelService: "s"})

	m.gcDisk(context.Background())

	if _, ok := m.Get(servicio); ok {
		t.Error("la congelada del servicio seguía: el GC debía recogerla")
	}
	for _, id := range []string{rama, suelta} {
		if _, ok := m.Get(id); !ok {
			t.Errorf("el GC borró %s, que no es de un servicio: su disco era la única copia de sus datos", id)
		}
	}
}
