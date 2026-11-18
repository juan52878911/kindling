package machine

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// S3: una copia de un dorado (run -from) que el reinicio pilló entre el
// Resume y el resync no se readopta como running: su VMM se para y queda
// failed. Ni en pausa ni ya reanudada.
func TestReconcileNoReadoptaUnaCopiaSinEntregar(t *testing.T) {
	for _, c := range []struct {
		nombre, estadoVMM string
		imagen            bool
	}{
		{"reanudada", "Running", false},
		{"en pausa", "Paused", false},
		{"de snapshot por imagen", "Running", true},
	} {
		t.Run(c.nombre, func(t *testing.T) {
			m := newTestManager(t)
			id := "c0b1a00000000001"
			mc := m.addForTest(id)
			if c.imagen {
				mc.ImageSnapshot = "img-dorado"
			} else {
				mc.From = "dorado"
			}
			falso, fin := vmmEnPausaTrasReinicio(t, m, id, api.StateCreated)
			falso.ponerEstado(c.estadoVMM)

			m.reconcile()

			if n := len(falso.llamadasA(http.MethodPatch, "/vm")); n != 0 {
				t.Fatalf("reconcile reanudó una copia sin resync (%d PATCH /vm)", n)
			}
			viva := vivaDe(t, m, id)
			if viva.State != api.StateFailed || viva.LastErr != errCopiaCortada || readoptada(m, id) {
				t.Fatalf("estado %s (%q), readoptada %v; quería failed sin readoptar", viva.State, viva.LastErr, readoptada(m, id))
			}
			select {
			case <-fin:
			case <-time.After(5 * time.Second):
				t.Fatal("el VMM de la copia cortada sigue vivo")
			}
		})
	}
}

// Una copia ya entregada alguna vez y que un Start vuelve a arrancar (en
// frío, created con StartedAt puesto) sí se readopta.
func TestReconcileReadoptaUnaCopiaArrancadaDeNuevo(t *testing.T) {
	m := newTestManager(t)
	id := "c0b1a00000000002"
	mc := m.addForTest(id)
	mc.From = "dorado"
	antes := time.Now().Add(-time.Hour)
	mc.StartedAt = &antes
	falso, fin := vmmEnPausaTrasReinicio(t, m, id, api.StateCreated)
	falso.ponerEstado("Running")

	m.reconcile()

	if viva := vivaDe(t, m, id); viva.State != api.StateRunning || !readoptada(m, id) {
		t.Fatalf("estado %s, readoptada %v; quería running readoptada", viva.State, readoptada(m, id))
	}
	if !sigueVivo(fin) {
		t.Fatal("reconcile mató el VMM de un arranque en frío")
	}
}

// F8: una copia que no se resincroniza se aborta y se borra; el reintento con
// el mismo -name no choca con una failed que se quedó con él.
func TestCopiaSinResyncSeBorraYLiberaElNombre(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "c0b1a00000000003"
	mc := m.addForTest(id)
	mc.Name, mc.From, mc.State = "copia", "dorado", api.StateCreated
	soltarCiclo := m.lockUnaVez(id)
	abortar := func(err error) (*api.Machine, error) {
		m.fail(mc, err)
		return nil, err
	}

	_, err := m.descartarCopia(id, soltarCiclo, abortar, errors.New("did not resync"))
	if err == nil || err.Error() != "did not resync" {
		t.Fatalf("error %v; quería el del resync", err)
	}
	// Borrada: su nombre pasa a la tumba en el acto, aunque la limpieza siga.
	if viva, ok := m.Get(id); ok && !esTumba(viva) {
		t.Fatalf("la copia sin resync sigue en el inventario (%s, %s)", viva.Name, viva.State)
	}
	soltar, err := m.reservarNombre("copia")
	if err != nil {
		t.Fatalf("reintentar con el mismo nombre: %v", err)
	}
	soltar()
}
