package machine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

// Una copia (run -from) que el reinicio pilló "created" y cuyo VMM ya no está
// (kill -9 entre persistirYa y spawn, o reinicio del host) no puede quedar
// zombi: failed con errCopiaCortada, red desmontada y nombre recogible.
func TestReproR6_CreatedSinVMMQuedaZombi(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "c0b1a000000000aa"
	mc := m.addForTest(id)
	hace := time.Now().Add(-48 * time.Hour)
	m.mu.Lock()
	mc.Name, mc.From, mc.State, mc.PID = "copia", "dorado", api.StateCreated, 0
	mc.TTLSeconds, mc.CreatedAt, mc.TTLAt, mc.OnTTL = 60, hace, &hace, api.OnTTLRemove
	m.mu.Unlock()

	desmontadas := 0
	cambiarGanchosRed(t, func(g *ganchosRed) { g.desmontar = func(*knet.Net) { desmontadas++ } })

	m.reconcile()

	viva := vivaDe(t, m, id)
	if viva.State != api.StateFailed || viva.LastErr != errCopiaCortada || viva.FailedAt == nil {
		t.Errorf("state=%s lastErr=%q failedAt=%v: debía quedar failed con errCopiaCortada", viva.State, viva.LastErr, viva.FailedAt)
	}
	if desmontadas == 0 {
		t.Errorf("reconcile no desmontó su red")
	}
}

// Una created sin VMM que no es copia sin entregar (arranque en frío) va a stopped.
func TestReconcileCreatedSinVMMEnFrioVaAStopped(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "c0b1a000000000bb"
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.Name, mc.State, mc.PID = "fria", api.StateCreated, 0
	m.mu.Unlock()
	desmontadas := 0
	cambiarGanchosRed(t, func(g *ganchosRed) { g.desmontar = func(*knet.Net) { desmontadas++ } })
	m.reconcile()
	if s := vivaDe(t, m, id).State; s != api.StateStopped {
		t.Errorf("state=%s, quería stopped", s)
	}
	if desmontadas != 1 {
		t.Errorf("desmontadas=%d, quería 1", desmontadas)
	}
}

// limpiarMaquina no borraba m.secretos[id].
func TestReproR6_SecretosNoSeOlvidanAlBorrar(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "5ec0000000000001"
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.State = api.StateStopped
	m.secretos = map[string]*estadoSecreto{id: {}}
	foto := mc.Clone()
	m.mu.Unlock()
	if err := m.limpiarMaquina(foto); err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	_, queda := m.secretos[id]
	_, existe := m.byID[id]
	m.mu.RUnlock()
	if existe {
		t.Fatal("la máquina sigue en byID")
	}
	if queda {
		t.Errorf("m.secretos conserva la entrada de %s tras borrarla", shortID(id))
	}
}

// A15: reconcile LIMPIA, no solo cambia el estado: por cada máquina sin VMM
// (created, running y warm) desmonta su red una vez y borra su cgroup.
func TestReconcileLimpiaRedYCgroupDeLasQueNoTienenVMM(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	m.cgroupRoot = t.TempDir()
	var n atomic.Int32
	cambiarGanchosRed(t, func(g *ganchosRed) { g.desmontar = func(*knet.Net) { n.Add(1) } })

	casos := map[string]api.State{
		"c1c10001aaaaaaaa": api.StateCreated,
		"c1c10002aaaaaaaa": api.StateRunning,
		"c1c10003aaaaaaaa": api.StateWarm,
	}
	for id, st := range casos {
		mc := m.addForTest(id)
		m.mu.Lock()
		mc.Name, mc.State, mc.PID = "m"+id[len(id)-1:], st, 0
		m.mu.Unlock()
		if err := os.Mkdir(filepath.Join(m.cgroupRoot, cgName(id)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m.reconcile()
	if got := n.Load(); got != int32(len(casos)) {
		t.Errorf("el desmontaje de la red se llamó %d veces, quería %d", got, len(casos))
	}
	for id, st := range casos {
		if existe(filepath.Join(m.cgroupRoot, cgName(id))) {
			t.Errorf("el cgroup de %s (%s) sigue en pie", shortID(id), st)
		}
	}
}

// R6-I2 (R6): un commit cortado tras releaseVolumes deja la plantilla sin
// volúmenes. Tras revertir el overlay y reanudar, reconcile se los devuelve
// (acquireVolumes); si no puede, la máquina queda failed, no running.
func TestReconcileCommitCortadoConVolumenesLosDevuelve(t *testing.T) {
	for _, falla := range []bool{false, true} {
		m := newTestManager(t)
		id := "a7a7000000000f06"
		_, _ = vmmEnPausaTrasReinicio(t, m, id, api.StateRunning)
		m.mu.Lock()
		m.byID[id].Volumes = []api.VolumeAttachment{{}}
		m.mu.Unlock()
		if err := marcarCommitEnCurso(m.dir(id), "dorado", 0); err != nil {
			t.Fatal(err)
		}
		var llamadas atomic.Int32
		old := devolverVolumenes
		devolverVolumenes = func(_ *Manager, mc *api.Machine) error {
			llamadas.Add(1)
			if falla {
				return errors.New("el invitado no monta")
			}
			return nil
		}
		t.Cleanup(func() { devolverVolumenes = old })

		m.reconcile()

		if llamadas.Load() != 1 {
			t.Fatalf("falla=%v: acquireVolumes llamado %d veces, quería 1", falla, llamadas.Load())
		}
		viva := vivaDe(t, m, id)
		if falla && viva.State != api.StateFailed {
			t.Fatalf("sin poder devolver los volúmenes quedó %s, quería failed", viva.State)
		}
		if !falla && viva.State != api.StateRunning {
			t.Fatalf("con los volúmenes devueltos quedó %s (%q), quería running", viva.State, viva.LastErr)
		}
	}
}

// R6-I2 (R29): un VMM que contesta «ocupado» está volcando; un rechazo claro
// no. Un plazo vencido tampoco (R3-5): no se sabe si vuelca o está colgado.
func TestVMMContestaOcupado(t *testing.T) {
	if !vmmContestaOcupado(errors.New("firecracker PATCH /drives/overlay: 400 Bad Request: Resource busy")) {
		t.Fatal("un VMM ocupado no se reconoció")
	}
	if vmmContestaOcupado(errors.New("firecracker PATCH /drives/overlay: 400 Bad Request: no")) {
		t.Fatal("un rechazo claro se tomó por ocupado")
	}
	if vmmContestaOcupado(fmt.Errorf("firecracker PATCH /drives/overlay: %w", context.DeadlineExceeded)) {
		t.Fatal("un plazo vencido se tomó por un VMM que contesta ocupado")
	}
}
