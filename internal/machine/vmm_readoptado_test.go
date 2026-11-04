package machine

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// vmmVivoDe lanza un proceso que liveVMs y adopt toman por el VMM de id:
// lleva "--api-sock <dir>/fc.sock" en su línea de órdenes (como $0 y $1 de
// sh, que espera con `read` sin lanzar hijos; ver
// TestLiveVMsEncuentraLaMaquinaPorSuSocket) y el socket existe. Lo recoge
// una goroutine: el canal se cierra cuando ha terminado. Si la plataforma no
// lo reconoce, el test se salta.
func vmmVivoDe(t *testing.T, m *Manager, id string) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(m.dir(id), "fc.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "read x", argSocketVMM, sock)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("no pude lanzar el proceso de prueba: %v", err)
	}
	fin := make(chan struct{})
	go func() { _ = cmd.Wait(); close(fin) }()
	t.Cleanup(func() {
		stdin.Close()
		_ = cmd.Process.Kill()
		<-fin
	})
	for i := 0; i < 400; i++ {
		if m.liveVMs()[id] == cmd.Process.Pid {
			if _, ok := m.adopt(&api.Machine{ID: id, PID: cmd.Process.Pid}); ok {
				return cmd, fin
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Skip("liveVMs/adopt no llegaron a reconocer el proceso de prueba")
	return nil, nil
}

// La marca del VMM que dejó vivo un Freeze sobrevive a un reinicio del
// daemon: reconcile no lo readopta (la máquina sigue warm, con su volcado),
// lo mata como huérfano y deja la marca en memoria para Thaw.
func TestReconcileNoReadoptaElVMMDeUnCongelado(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "c0c0000000000001"
	mc := congeladaParaThaw(t, m, id, 64)
	cmd, fin := vmmVivoDe(t, m, id)
	pid := cmd.Process.Pid

	m.apuntarVMMDeCongelado(id, pid)
	b, err := os.ReadFile(filepath.Join(m.dir(id), ficheroVMMCongelado))
	if err != nil || strings.TrimSpace(string(b)) != strconv.Itoa(pid) {
		t.Fatalf("marca en disco = %q (%v); quería el pid %d", b, err, pid)
	}
	// Lo que ve un daemon nuevo: nada en memoria, solo el disco.
	m.vmmDeCongelado.Delete(id)

	m.reconcile()

	viva := vivaDe(t, m, mc.ID)
	if viva.State != api.StateWarm || viva.PID != 0 {
		t.Fatalf("tras reconcile: estado %s, PID %d; quería warm sin PID", viva.State, viva.PID)
	}
	if err := volcadoValido(m.dir(id), nil); err != nil {
		t.Fatalf("el volcado dejó de valer: %v", err)
	}
	if v, ok := m.vmmDeCongelado.Load(id); !ok || v.(int) != pid {
		t.Fatalf("marca en memoria = %v, %v; quería %d para que Thaw lo mate", v, ok, pid)
	}
	m.mu.RLock()
	_, conSocket := m.socket[id]
	m.mu.RUnlock()
	if conSocket {
		t.Fatal("reconcile le apuntó un socket: lo readoptó")
	}
	select {
	case <-fin:
	case <-time.After(5 * time.Second):
		t.Fatal("killOrphanVMMs no mató el VMM del congelado")
	}
}

// Una marca en disco de otro PID (o de uno que ya no vive) no es de nadie:
// no impide readoptar y se borra.
func TestMarcaDeCongeladoDeOtroPIDSeBorra(t *testing.T) {
	m := newTestManager(t)
	id := "c0c0000000000002"
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	m.apuntarVMMDeCongelado(id, 4242)
	m.vmmDeCongelado.Delete(id)
	if m.vmmCongeladoEnDisco(id, 4243) {
		t.Fatal("una marca de otro pid no es la de este VMM")
	}
	if _, err := os.Stat(filepath.Join(m.dir(id), ficheroVMMCongelado)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("la marca vieja sigue en disco (%v)", err)
	}
	if _, ok := m.vmmDeCongelado.Load(id); ok {
		t.Fatal("una marca vieja no vuelve a la memoria")
	}

	// thawVMMViejo, al matarlo, la quita también del disco.
	m.apuntarVMMDeCongelado(id, 4242)
	m.olvidarVMMDeCongelado(id)
	if _, err := os.Stat(filepath.Join(m.dir(id), ficheroVMMCongelado)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("olvidar no borró la marca del disco (%v)", err)
	}
}

// Thaw que readopta un VMM vivo lo vigila: su muerte marca la máquina failed
// en el acto, sin esperar a la vuelta del vigilante (aquí ni corre).
func TestThawQueReadoptaVigilaElVMM(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	m.priv = &Privileges{}
	id := "c0c0000000000003"
	congeladaParaThaw(t, m, id, 64)
	cmd, fin := vmmVivoDe(t, m, id)

	out, err := m.Thaw(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != api.StateRunning || out.PID != cmd.Process.Pid {
		t.Fatalf("Thaw = %s pid %d; quería running readoptando %d", out.State, out.PID, cmd.Process.Pid)
	}
	_ = cmd.Process.Signal(syscall.SIGKILL)
	<-fin
	limite := time.Now().Add(3 * time.Second)
	for {
		if mc := vivaDe(t, m, id); mc.State == api.StateFailed {
			if mc.LastErr != errProcesoDesaparecido || mc.PID != 0 {
				t.Fatalf("failed con err %q pid %d", mc.LastErr, mc.PID)
			}
			return
		}
		if time.Now().After(limite) {
			t.Fatalf("estado = %s 3 s después de morir el VMM readoptado; nadie lo vigilaba", vivaDe(t, m, id).State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// La salida de un VMM readoptado dice por qué no trae código de salida y lo
// que dejó el VMM en su consola, como la de uno hijo.
func TestSalidaDeReadoptadoDiceElMotivo(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	sub, baja := m.bus.Subscribe()
	defer baja()
	id := "c0c0000000000004"
	cmd, fin := vmmVivoDe(t, m, id)
	corriendoCon(m, id, cmd.Process.Pid)
	consola := filepath.Join(m.dir(id), "firecracker.log")
	if err := os.WriteFile(consola, []byte("[    1.0] reboot: Restarting system\nkling-hv: the guest stopped: the guest rebooted (SYSTEM_RESET)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	var mu sync.Mutex
	prev := log.Writer()
	log.SetOutput(escritorSeguro{&mu, &buf})
	t.Cleanup(func() { log.SetOutput(prev) })
	m.vigilarAdoptado(id, cmd.Process.Pid)
	_ = cmd.Process.Signal(syscall.SIGKILL)
	<-fin

	select {
	case ev := <-sub:
		if ev.Type != api.EvFailed || !strings.Contains(ev.Message, detalleAdoptado) ||
			!strings.Contains(ev.Message, "the guest rebooted") {
			t.Fatalf("evento = %+v; quería EvFailed con %q y el motivo de la consola", ev, detalleAdoptado)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no se publicó EvFailed: nadie vigilaba al readoptado")
	}
	mu.Lock()
	l := buf.String()
	mu.Unlock()
	if !strings.Contains(l, "exited ("+detalleAdoptado+"; the guest stopped: the guest rebooted") {
		t.Fatalf("log = %q; quería la salida readoptada con el motivo", l)
	}
}
