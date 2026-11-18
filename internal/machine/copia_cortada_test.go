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

// F20: una copia que el arranque dejó failed sin entregarla, con su VMM
// aún vivo, tampoco se readopta: antes pasaba a running sin resync. Y
// conserva el error por el que falló.
func TestReconcileNoReadoptaUnaCopiaFallidaSinEntregar(t *testing.T) {
	m := newTestManager(t)
	id := "c0b1a00000000003"
	mc := m.addForTest(id)
	mc.From = "dorado"
	mc.LastErr = "resync failed"
	falso, fin := vmmEnPausaTrasReinicio(t, m, id, api.StateFailed)
	falso.ponerEstado("Running")

	m.reconcile()

	viva := vivaDe(t, m, id)
	if viva.State != api.StateFailed || viva.LastErr != "resync failed" || readoptada(m, id) {
		t.Fatalf("estado %s (%q), readoptada %v; quería failed con su error, sin readoptar",
			viva.State, viva.LastErr, readoptada(m, id))
	}
	select {
	case <-fin:
	case <-time.After(5 * time.Second):
		t.Fatal("el VMM de la copia fallida sigue vivo")
	}
}

// F6: un VMM vivo que no llegó a arrancar invitado (estado "Not started": el
// reinicio cortó un thaw antes de cargar) no se readopta como running. En un
// thaw, el volcado no se cargó: su sello vuelve y la máquina sigue warm.
func TestReconcileNoReadoptaUnVMMSinArrancar(t *testing.T) {
	t.Run("thaw cortado", func(t *testing.T) {
		m := newTestManager(t)
		id := "a7a7000000000021"
		congeladaParaThaw(t, m, id, 64)
		falso, fin := vmmEnPausaTrasReinicio(t, m, id, api.StateWarm)
		falso.ponerEstado(estadoNoArrancado)
		if err := retirarSello(m.dir(id)); err != nil {
			t.Fatal(err)
		}

		m.reconcile()

		if viva := vivaDe(t, m, id); viva.State != api.StateWarm || readoptada(m, id) {
			t.Fatalf("estado %s, readoptada %v; quería warm sin readoptar", viva.State, readoptada(m, id))
		}
		if err := volcadoValido(m.dir(id), nil); err != nil {
			t.Fatalf("el volcado que nadie cargó dejó de valer: %v", err)
		}
		select {
		case <-fin:
		case <-time.After(5 * time.Second):
			t.Fatal("el VMM sin arrancar sigue vivo")
		}
	})
	t.Run("figura running", func(t *testing.T) {
		m := newTestManager(t)
		id := "a7a7000000000022"
		falso, fin := vmmEnPausaTrasReinicio(t, m, id, api.StateRunning)
		falso.ponerEstado(estadoNoArrancado)

		m.reconcile()

		if viva := vivaDe(t, m, id); viva.State == api.StateRunning || readoptada(m, id) {
			t.Fatalf("estado %s, readoptada %v; quería sin readoptar", viva.State, readoptada(m, id))
		}
		select {
		case <-fin:
		case <-time.After(5 * time.Second):
			t.Fatal("el VMM sin arrancar sigue vivo")
		}
	})
}
