package machine

import (
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// procesoVivo lanza un proceso que espera, y lo recoge cuando muere (como la
// goroutine de cmd.Wait() del arranque: sin ella se quedaría zombi y waitGone
// esperaría su tope entero).
func procesoVivo(t *testing.T) (pid int, muerto <-chan struct{}) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	if err := cmd.Start(); err != nil {
		t.Skipf("no pude lanzar el proceso de prueba: %v", err)
	}
	ch := make(chan struct{})
	go func() { _ = cmd.Wait(); close(ch) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-ch })
	return cmd.Process.Pid, ch
}

// killMachine leía el State de la entrada VIVA sin m.mu mientras otros la
// escriben con él (con -race salta). Y dejaba el PID puesto: un segundo kill
// concurrente, que ya lo había leído, mandaba su SIGKILL a un PID que tras
// morir el primero podía ser de otro proceso. El que mata deja PID 0.
func TestKillMachineSinCarrerasYDejaElPIDaCero(t *testing.T) {
	m := newTestManager(t)
	id := newID()
	pid, muerto := procesoVivo(t)
	m.byID[id] = &api.Machine{ID: id, Name: "x", State: api.StateRunning, PID: pid}

	parar := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-parar:
				return
			default:
			}
			m.mu.Lock()
			m.byID[id].State = api.StateRunning
			m.byID[id].Ready = api.ReadyWaiting
			m.mu.Unlock()
		}
	}()
	var kills sync.WaitGroup
	for i := 0; i < 2; i++ {
		kills.Add(1)
		go func() { defer kills.Done(); m.kill(id) }()
	}
	kills.Wait()
	close(parar)
	wg.Wait()

	select {
	case <-muerto:
	case <-time.After(5 * time.Second):
		t.Fatal("kill no mató el proceso")
	}
	m.mu.RLock()
	quedo := m.byID[id].PID
	m.mu.RUnlock()
	if quedo != 0 {
		t.Fatalf("tras matar, PID = %d: otro kill podría mandarle SIGKILL a un PID reciclado", quedo)
	}
}

// giveUpOn (el TTL rindiéndose con una máquina) mataba sin el cerrojo de ciclo
// de vida lo que hubiera en ese momento. Si mientras decidía la relanzaron
// (otro VMM, otro PID), mataba el nuevo, sano.
func TestGiveUpOnNoMataUnaMaquinaRelanzadaEntreMedias(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := newID()
	m.byID[id] = &api.Machine{ID: id, Name: "sorda", State: api.StateRunning, PID: pidMuerto(t)}
	visto, _ := m.Get(id)

	soltar := m.lock(id) // un Stop + Thaw en curso
	hecho := make(chan struct{})
	go func() {
		m.giveUpOn(visto, errors.New("its control socket is gone"))
		close(hecho)
	}()
	esperarQueEspere(t, m, id)
	nuevo, muerto := procesoVivo(t)
	m.mu.Lock()
	m.byID[id].PID = nuevo
	m.mu.Unlock()
	soltar()
	select {
	case <-hecho:
	case <-time.After(10 * time.Second):
		t.Fatal("giveUpOn no terminó")
	}
	if mc, _ := m.Get(id); mc.State != api.StateRunning {
		t.Fatalf("estado = %s; la relanzada debía seguir running", mc.State)
	}
	select {
	case <-muerto:
		t.Fatal("giveUpOn mató el VMM nuevo de una máquina relanzada")
	default:
	}
}
