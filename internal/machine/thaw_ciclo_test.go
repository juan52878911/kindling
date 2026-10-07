package machine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// congeladaParaThaw registra una máquina warm con un volcado sellado de pega:
// lo justo para que Thaw pase sus comprobaciones y llegue a la admisión y a
// la puerta de arranque sin KVM.
func congeladaParaThaw(t *testing.T, m *Manager, id string, memMiB int) *api.Machine {
	t.Helper()
	mc := m.addForTest(id)
	m.mu.Lock()
	m.byID[id].State = api.StateWarm
	m.byID[id].MemMiB = memMiB
	m.byID[id].VCPUs = 1
	m.mu.Unlock()
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"snap.file", "mem.file"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("volcado "+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := volcadoEnCurso(dir); err != nil {
		t.Fatal(err)
	}
	if err := sellarVolcado(dir, "", "", ""); err != nil {
		t.Fatal(err)
	}
	return mc
}

func pendiente(m *Manager) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.pendingMiB
}

// fingirMemoria sustituye la memoria disponible del host mientras dure el test.
func fingirMemoria(t *testing.T, mib int) {
	t.Helper()
	old := availableMiB
	availableMiB = func() int { return mib }
	t.Cleanup(func() { availableMiB = old })
}

// Thaw no pasaba por la admisión de memoria: una tormenta de thaws devolvía
// al host la RAM de todos sus invitados sin que nada dijera que no. Ahora
// rechaza con 507 y no deja la reserva colgada.
func TestThawPasaPorLaAdmisionDeMemoria(t *testing.T) {
	t.Setenv("KLING_MAX_MEM_PRESSURE", "0")
	t.Setenv("KLING_MAX_SWAP_PCT", "0")
	t.Setenv("KLING_MIN_FREE_MIB", "0")
	m := newTestManager(t)
	mc := congeladaParaThaw(t, m, "a1a1000000000001", 512)

	// El host no tiene sitio: 100 MiB para una de 512.
	fingirMemoria(t, 100)
	_, err := m.Thaw(context.Background(), mc.ID)
	if !api.IsInsufficientMemory(err) {
		t.Fatalf("Thaw sin memoria = %v; quería un 507", err)
	}
	if p := pendiente(m); p != 0 {
		t.Fatalf("pendingMiB = %d tras un thaw rechazado; la reserva quedó colgada", p)
	}
	if got := vivaDe(t, m, mc.ID).State; got != api.StateWarm {
		t.Fatalf("estado = %s tras el rechazo, quería frozen", got)
	}

	// Con sitio, la reserva existe mientras espera la puerta de arranque y se
	// suelta al salir.
	fingirMemoria(t, 1<<30)
	m.launchGate = make(chan struct{}, 1)
	m.launchGate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() {
		_, err := m.Thaw(ctx, mc.ID)
		res <- err
	}()
	limite := time.Now().Add(5 * time.Second)
	for pendiente(m) != 512 {
		if time.Now().After(limite) {
			cancel()
			t.Fatalf("pendingMiB = %d esperando la puerta; quería 512 reservados", pendiente(m))
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-res; !errors.Is(err, context.Canceled) {
		t.Fatalf("Thaw = %v; quería context.Canceled en la puerta", err)
	}
	if p := pendiente(m); p != 0 {
		t.Fatalf("pendingMiB = %d al salir; quería 0", p)
	}
}

// Thaw seguía con la copia de antes del cerrojo si la máquina se borró
// mientras esperaba: lanzaba un VMM para algo que ya no existe. Como Freeze y
// Commit, ahora lo dice, y como un "no existe".
func TestThawDeUnaBorradaMientrasEsperaba(t *testing.T) {
	t.Setenv("KLING_MAX_MEM_PRESSURE", "0")
	m := newTestManager(t)
	m.bus = events.New()
	mc := congeladaParaThaw(t, m, "a1a1000000000002", 64)

	soltar := m.lock(mc.ID)
	res := make(chan error, 1)
	go func() {
		_, err := m.Thaw(context.Background(), mc.ID)
		res <- err
	}()
	esperarQueEspere(t, m, mc.ID)
	m.mu.Lock()
	delete(m.byID, mc.ID)
	m.mu.Unlock()
	soltar()

	select {
	case err := <-res:
		if err == nil || !strings.Contains(err.Error(), "doesn't exist") {
			t.Fatalf("Thaw de una borrada = %v; quería que dijera que no existe", err)
		}
		// Y que sea un "no existe" de verdad: el daemon lo contesta con un
		// 404, que es como el planificador (pkg/scheduler/aislada.go) da por
		// perdida una sesión aislada. Con un error sin marcar era un 400.
		if !errors.Is(err, ErrNoMachine) {
			t.Fatalf("Thaw de una borrada = %v; quería ErrNoMachine", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Thaw no volvió")
	}
}

// Readoptar un VMM que ya corría (estado obsoleto) no reaplicaba el techo de
// CPU: tras un reinicio del daemon su cgroup ya no estaba y corría sin límite.
func TestThawQueReadoptaReaplicaElTechoDeCPU(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "a1a1000000000003"
	pid, _ := vmmFalso(t, m, id)
	m.cgroupRoot = t.TempDir()
	m.addForTest(id)
	m.mu.Lock()
	m.byID[id].State = api.StateWarm
	m.byID[id].VCPUs = 1
	m.byID[id].CPUPct = 50
	m.mu.Unlock()

	out, err := m.Thaw(context.Background(), id)
	if err != nil {
		t.Fatalf("Thaw: %v", err)
	}
	if out.State != api.StateRunning || out.PID != pid {
		t.Fatalf("readopción: estado %s pid %d, quería running con %d", out.State, out.PID, pid)
	}
	procs, err := os.ReadFile(filepath.Join(m.dirCgroup(id), "cgroup.procs"))
	if err != nil || strings.TrimSpace(string(procs)) != strconv.Itoa(pid) {
		t.Fatalf("cgroup.procs = %q (%v); el VMM readoptado no volvió a su cgroup", procs, err)
	}
	max, _ := os.ReadFile(filepath.Join(m.dirCgroup(id), "cpu.max"))
	if string(max) != "50000 100000" {
		t.Fatalf("cpu.max = %q; quería el techo configurado (50 %%)", max)
	}
}
