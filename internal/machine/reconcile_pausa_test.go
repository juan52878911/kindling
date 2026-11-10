package machine

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// A7: un reinicio del daemon a mitad de un Freeze o de un Commit deja un VMM
// vivo con el invitado en pausa y la máquina running en disco. reconcile le
// pregunta el estado (GET /) antes de readoptarlo.

// vmmEnPausaTrasReinicio deja lo que ve un daemon nuevo: la máquina id en
// estado st en disco, su VMM vivo y un fcFalso que contesta GET / con
// "Paused". reconcile habla con el falso (pruebasSocketVMM).
func vmmEnPausaTrasReinicio(t *testing.T, m *Manager, id string, st api.State) (*fcFalso, <-chan struct{}) {
	t.Helper()
	m.bus = events.New()
	if m.byID[id] == nil {
		m.addForTest(id)
	}
	m.mu.Lock()
	m.byID[id].State = st
	m.mu.Unlock()
	cmd, fin := vmmVivoDe(t, m, id)
	m.mu.Lock()
	m.byID[id].PID = cmd.Process.Pid
	m.mu.Unlock()
	falso := nuevoFcFalso(t)
	falso.ponerEstado("Paused")
	m.pruebasSocketVMM = func(string) string { return falso.Sock }
	return falso, fin
}

func readoptada(m *Manager, id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.socket[id]
	return ok
}

// Pausada figurando running y sin volcado que valga (un Freeze cortado antes
// del sello, o una pausa a medias): se reanuda y se readopta.
func TestReconcileReanudaUnVMMEnPausaQueFiguraRunning(t *testing.T) {
	m := newTestManager(t)
	id := "a7a7000000000001"
	falso, fin := vmmEnPausaTrasReinicio(t, m, id, api.StateRunning)
	// La marca de un volcado en curso, sin sello: lo que deja Freeze.
	if err := volcadoEnCurso(m.dir(id)); err != nil {
		t.Fatal(err)
	}

	m.reconcile()

	if st := falso.verEstado(); st != "Running" {
		t.Fatalf("el VMM sigue %s: reconcile no lo reanudó", st)
	}
	if viva := vivaDe(t, m, id); viva.State != api.StateRunning || !readoptada(m, id) {
		t.Fatalf("estado %s, readoptada %v; quería running readoptada", viva.State, readoptada(m, id))
	}
	if !sigueVivo(fin) {
		t.Fatal("reconcile mató el VMM de una máquina que podía seguir")
	}
}

// Si la reanudación falla, queda running con la transición freezing y
// pendiente para el vigilante: el planificador no le enruta nada.
func TestReconcileSinPoderReanudarLaDejaPendiente(t *testing.T) {
	m := newTestManager(t)
	id := "a7a7000000000002"
	falso, _ := vmmEnPausaTrasReinicio(t, m, id, api.StateRunning)
	falso.fallar(http.MethodPatch, "/vm", http.StatusBadRequest, "busy")

	m.reconcile()

	m.mu.RLock()
	_, pendiente := m.reanudaciones[id]
	tr := m.transicion[id]
	m.mu.RUnlock()
	if !pendiente || tr != api.TransitionFreezing {
		t.Fatalf("pendiente %v, transición %q; quería pendiente y freezing", pendiente, tr)
	}
}

// Con la marca de commit, el disco del VMM es el overlay dorado: no se
// readopta ni se reanuda; muere y la máquina queda failed.
func TestReconcileNoReadoptaUnCommitCortado(t *testing.T) {
	m := newTestManager(t)
	id := "a7a7000000000003"
	falso, fin := vmmEnPausaTrasReinicio(t, m, id, api.StateRunning)
	if err := marcarCommitEnCurso(m.dir(id), "dorado"); err != nil {
		t.Fatal(err)
	}

	m.reconcile()

	if n := len(falso.llamadasA(http.MethodPatch, "/vm")); n != 0 {
		t.Fatalf("reconcile pidió %d PATCH /vm a un VMM que escribe en el dorado", n)
	}
	viva := vivaDe(t, m, id)
	if viva.State != api.StateFailed || !strings.Contains(viva.LastErr, "commit") || readoptada(m, id) {
		t.Fatalf("estado %s (%q), readoptada %v; quería failed sin readoptar", viva.State, viva.LastErr, readoptada(m, id))
	}
	select {
	case <-fin:
	case <-time.After(5 * time.Second):
		t.Fatal("killOrphanVMMs no mató el VMM del commit cortado")
	}
	// Muerto el VMM, el siguiente reinicio retira la marca.
	m.reconcile()
	if existe(filepath.Join(m.dir(id), marcaCommit)) {
		t.Fatal("la marca de commit sigue con el VMM ya muerto")
	}
}

// También con la máquina en pausa en disco (commitPausada): la marca manda.
func TestReconcileNoReadoptaUnCommitCortadoDeUnaPausada(t *testing.T) {
	m := newTestManager(t)
	id := "a7a7000000000004"
	_, fin := vmmEnPausaTrasReinicio(t, m, id, api.StatePaused)
	if err := marcarCommitEnCurso(m.dir(id), "dorado"); err != nil {
		t.Fatal(err)
	}
	m.reconcile()
	if viva := vivaDe(t, m, id); viva.State != api.StateFailed {
		t.Fatalf("estado %s; quería failed", viva.State)
	}
	select {
	case <-fin:
	case <-time.After(5 * time.Second):
		t.Fatal("el VMM del commit cortado sigue vivo")
	}
}

// Pausada figurando running con su volcado sellado: un Freeze que selló y no
// llegó a escribir warm. Reanudarla haría divergir el disco del volcado: queda
// warm y el VMM muere.
func TestReconcileConVolcadoSelladoLaDejaWarm(t *testing.T) {
	m := newTestManager(t)
	id := "a7a7000000000005"
	congeladaParaThaw(t, m, id, 64)
	falso, fin := vmmEnPausaTrasReinicio(t, m, id, api.StateRunning)

	m.reconcile()

	if n := len(falso.llamadasA(http.MethodPatch, "/vm")); n != 0 {
		t.Fatalf("reconcile reanudó un VMM cuyo volcado sigue sellado (%d PATCH /vm)", n)
	}
	if viva := vivaDe(t, m, id); viva.State != api.StateWarm || readoptada(m, id) {
		t.Fatalf("estado %s, readoptada %v; quería warm sin readoptar", viva.State, readoptada(m, id))
	}
	if err := volcadoValido(m.dir(id), nil); err != nil {
		t.Fatalf("el volcado dejó de valer: %v", err)
	}
	select {
	case <-fin:
	case <-time.After(5 * time.Second):
		t.Fatal("el VMM pausado del congelado sigue vivo")
	}
}

// Despierta figurando en pausa (un Resume cortado antes de persistir): running.
func TestReconcileDespiertaQueFiguraEnPausaPasaARunning(t *testing.T) {
	m := newTestManager(t)
	id := "a7a7000000000006"
	falso, _ := vmmEnPausaTrasReinicio(t, m, id, api.StatePaused)
	falso.ponerEstado("Running")
	m.reconcile()
	if viva := vivaDe(t, m, id); viva.State != api.StateRunning {
		t.Fatalf("estado %s; quería running", viva.State)
	}
}

// Commit deja la marca mientras el disco apunta al dorado y la retira al
// devolverle el suyo.
func TestCommitMarcaElReapuntadoAlDorado(t *testing.T) {
	puedePerforar(t)
	m := newTestManager(t)
	id := "a7a7000000000007"
	falso, _ := plantillaParaCommit(t, m, id)
	dir := m.snapDir("dorado")
	marca := filepath.Join(m.dir(id), marcaCommit)
	conMarca := false
	falso.enGancho(func(metodo, ruta string) {
		if ruta == "/snapshot/create" {
			_, err := os.Stat(marca)
			conMarca = err == nil
			volcadoFalso(m, id, dir)
		}
	})
	if _, err := m.Commit(t.Context(), id, "dorado", false); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !conMarca {
		t.Fatal("el volcado del commit corrió sin la marca en disco")
	}
	if existe(marca) {
		t.Fatal("la marca sigue tras devolverle el disco a la plantilla")
	}
}
