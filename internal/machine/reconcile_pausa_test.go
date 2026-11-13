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
	if err := marcarCommitEnCurso(m.dir(id), "dorado", 0); err != nil {
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
	if err := marcarCommitEnCurso(m.dir(id), "dorado", 0); err != nil {
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

// A3: la marca de un commit cortado es del VMM que lo hacía. reconcile la
// atiende con ese VMM vivo; con otro, arrancado después (kling start, como
// pide el error), la retira y readopta la máquina. Antes la marca sobrevivía
// y el siguiente reinicio del daemon mataba a una máquina sana.
func TestMarcaDeCommitNoMataAlVMMDeUnArranquePosterior(t *testing.T) {
	m := newTestManager(t)
	id := "a7a7000000000008"
	_, fin := vmmEnPausaTrasReinicio(t, m, id, api.StateRunning)
	m.mu.RLock()
	pidCommit := m.byID[id].PID
	m.mu.RUnlock()
	if err := marcarCommitEnCurso(m.dir(id), "dorado", pidCommit); err != nil {
		t.Fatal(err)
	}
	m.reconcile()
	if viva := vivaDe(t, m, id); viva.State != api.StateFailed {
		t.Fatalf("con el VMM del commit vivo: estado %s; quería failed", viva.State)
	}
	select {
	case <-fin:
	case <-time.After(5 * time.Second):
		t.Fatal("killOrphanVMMs no mató el VMM del commit cortado")
	}

	// Otro VMM, de un arranque posterior, con la marca aún en disco.
	falso, _ := vmmEnPausaTrasReinicio(t, m, id, api.StateRunning)
	falso.ponerEstado("Running")
	m.reconcile()
	if viva := vivaDe(t, m, id); viva.State != api.StateRunning || !readoptada(m, id) {
		t.Fatalf("con un VMM nuevo: estado %s (%q), readoptada %v; quería running readoptada",
			viva.State, viva.LastErr, readoptada(m, id))
	}
	if existe(filepath.Join(m.dir(id), marcaCommit)) {
		t.Fatal("la marca de otro VMM sigue en disco")
	}
}

// Las marcas sin PID (las de antes, aún en disco) siguen valiendo para
// cualquier VMM: es lo seguro con un disco que pudo quedar en el dorado.
func TestMarcaDeCommitSinPIDSigueValiendo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, marcaCommit), []byte("dorado\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if hay, pid := marcaCommitDe(dir); !hay || pid != 0 {
		t.Fatalf("marca vieja: hay %v, pid %d; quería hay sin PID", hay, pid)
	}
	if err := marcarCommitEnCurso(dir, "dorado", 4321); err != nil {
		t.Fatal(err)
	}
	if hay, pid := marcaCommitDe(dir); !hay || pid != 4321 {
		t.Fatalf("marca nueva: hay %v, pid %d; quería 4321", hay, pid)
	}
}

// Start y el thaw de una failed retiran la marca una vez muerto el VMM viejo.
func TestStartYThawRetiranLaMarcaDeCommit(t *testing.T) {
	m := newTestManager(t)
	m.fcBin = filepath.Join(t.TempDir(), "no-hay-vmm")
	mc, _ := paradaParaStart(t, m, "a7a7000000000009")
	fallidaConVolcado(t, m, mc)
	marca := filepath.Join(m.dir(mc.ID), marcaCommit)

	if err := marcarCommitEnCurso(m.dir(mc.ID), "dorado", 0); err != nil {
		t.Fatal(err)
	}
	viva := vivaDe(t, m, mc.ID)
	if err := m.thawFallida(&viva); err != nil {
		t.Fatalf("thawFallida: %v", err)
	}
	if existe(marca) {
		t.Fatal("el thaw de una failed dejó la marca de commit")
	}

	if err := marcarCommitEnCurso(m.dir(mc.ID), "dorado", 0); err != nil {
		t.Fatal(err)
	}
	// Sin VMM que lanzar: el arranque falla, pero después de matar al viejo.
	if _, err := m.Start(t.Context(), mc.ID, nil); err == nil {
		t.Fatal("Start sin VMM no devolvió error")
	}
	if existe(marca) {
		t.Fatal("Start dejó la marca de commit")
	}
}
