package machine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// vmmQueSeVe lanza un proceso que adopt reconoce como el VMM de id: su línea
// de órdenes lleva el fc.sock de la máquina (en $0 de sh, que sigue vivo por
// el ": " de detrás) y el socket existe. No lo recoge nadie: quien llama le
// pone esperarVMM o su propio Wait.
func vmmQueSeVe(t *testing.T, m *Manager, id string) *exec.Cmd {
	t.Helper()
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(m.dir(id), "fc.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "sleep 30; :", sock)
	if err := cmd.Start(); err != nil {
		t.Skipf("no pude lanzar el proceso de prueba: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	for i := 0; i < 200; i++ {
		if _, ok := m.adopt(&api.Machine{ID: id, PID: cmd.Process.Pid}); ok {
			return cmd
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Skip("adopt no llegó a reconocer el proceso de prueba")
	return nil
}

// corriendoCon registra id como running con el VMM pid.
func corriendoCon(m *Manager, id string, pid int) {
	if m.bus == nil {
		m.bus = events.New()
	}
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.PID = pid
	m.mu.Unlock()
}

func estadoDeId(t *testing.T, m *Manager, id string) api.Machine {
	t.Helper()
	m.mu.RLock()
	defer m.mu.RUnlock()
	mc := m.byID[id]
	if mc == nil {
		t.Fatalf("%s ya no existe", id)
	}
	return *mc
}

// El caso de la auditoría: kill -9 del VMM con la máquina ociosa. La máquina
// pasa a failed en cuanto el proceso muere, sin esperar a la vuelta del
// vigilante (aquí ni siquiera corre), con su motivo y su evento.
func TestVMMQueMuereSeApuntaFailedEnElActo(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	sub, baja := m.bus.Subscribe()
	defer baja()
	id := "dead0000000000a1"
	cmd := vmmQueSeVe(t, m, id)
	corriendoCon(m, id, cmd.Process.Pid)
	logf, err := os.CreateTemp(t.TempDir(), "consola")
	if err != nil {
		t.Fatal(err)
	}
	hecho := make(chan struct{})
	go func() { m.esperarVMM(id, cmd, logf); close(hecho) }()

	inicio := time.Now()
	_ = cmd.Process.Signal(syscall.SIGKILL)
	select {
	case <-hecho:
	case <-time.After(5 * time.Second):
		t.Fatal("esperarVMM no volvió tras matar el proceso")
	}
	mc := estadoDeId(t, m, id)
	if mc.State != api.StateFailed || mc.LastErr != errProcesoDesaparecido || mc.PID != 0 || mc.FailedAt == nil {
		t.Fatalf("tras el kill -9: state=%s err=%q pid=%d failedAt=%v; quería failed, %q, 0 y con fecha",
			mc.State, mc.LastErr, mc.PID, mc.FailedAt, errProcesoDesaparecido)
	}
	if d := time.Since(inicio); d > 2*time.Second {
		t.Fatalf("tardó %v en verse; tiene que ser en el acto, no a la vuelta del vigilante", d)
	}
	select {
	case ev := <-sub:
		if ev.Type != api.EvFailed || ev.ID != id || !strings.Contains(ev.Message, "killed") {
			t.Fatalf("evento = %+v; quería EvFailed de %s diciendo cómo murió", ev, id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no se publicó EvFailed")
	}
}

// Un VMM que muere porque alguien lo mató a propósito (Stop, Freeze, rm) no
// marca nada: esos caminos lo matan con el cerrojo de ciclo de vida tomado y
// lo sueltan con el estado ya cambiado, y vmmTermino espera a ese cerrojo.
func TestVMMQueMuereEnUnStopNoLaMarcaFailed(t *testing.T) {
	m := newTestManager(t)
	id := "dead0000000000a2"
	cmd := vmmQueSeVe(t, m, id)
	corriendoCon(m, id, cmd.Process.Pid)
	logf, err := os.CreateTemp(t.TempDir(), "consola")
	if err != nil {
		t.Fatal(err)
	}
	hecho := make(chan struct{})
	go func() { m.esperarVMM(id, cmd, logf); close(hecho) }()

	// Lo que hace Stop: cerrojo, matar, esperar a que muera y apuntar stopped.
	soltar := m.lock(id)
	_ = cmd.Process.Signal(syscall.SIGKILL)
	time.Sleep(100 * time.Millisecond) // que esperarVMM llegue a esperar el cerrojo
	m.mu.Lock()
	ahora := time.Now()
	m.byID[id].State, m.byID[id].PID, m.byID[id].StoppedAt = api.StateStopped, 0, &ahora
	m.mu.Unlock()
	soltar()

	select {
	case <-hecho:
	case <-time.After(5 * time.Second):
		t.Fatal("esperarVMM no volvió")
	}
	if mc := estadoDeId(t, m, id); mc.State != api.StateStopped || mc.LastErr != "" {
		t.Fatalf("tras un stop: state=%s err=%q; quería stopped sin error", mc.State, mc.LastErr)
	}
}

// Si el que termina es un VMM anterior de la máquina (se relanzó y el viejo
// tardó en irse), la máquina sigue con el nuevo y no se toca.
func TestVMMViejoQueTerminaNoTumbaAlNuevo(t *testing.T) {
	m := newTestManager(t)
	id := "dead0000000000a3"
	nuevo := vmmQueSeVe(t, m, id)
	corriendoCon(m, id, nuevo.Process.Pid)
	viejo := exec.Command("/bin/sh", "-c", "exit 0")
	if err := viejo.Start(); err != nil {
		t.Skip(err)
	}
	logf, err := os.CreateTemp(t.TempDir(), "consola")
	if err != nil {
		t.Fatal(err)
	}
	m.esperarVMM(id, viejo, logf)
	if mc := estadoDeId(t, m, id); mc.State != api.StateRunning || mc.PID != nuevo.Process.Pid {
		t.Fatalf("state=%s pid=%d; quería running con el VMM nuevo (%d)", mc.State, mc.PID, nuevo.Process.Pid)
	}
}

// Un VMM readoptado tras reiniciar el daemon no es hijo suyo: su salida se
// espera con kqueue (macOS) o pidfd (Linux).
func TestVMMReadoptadoQueMuereSeApuntaFailed(t *testing.T) {
	m := newTestManager(t)
	id := "dead0000000000a4"
	cmd := vmmQueSeVe(t, m, id)
	recogido := make(chan struct{})
	go func() { _ = cmd.Wait(); close(recogido) }() // el "init" que lo recoge
	corriendoCon(m, id, cmd.Process.Pid)
	m.vigilarAdoptado(id, cmd.Process.Pid)

	_ = cmd.Process.Signal(syscall.SIGKILL)
	<-recogido
	limite := time.Now().Add(3 * time.Second)
	for time.Now().Before(limite) {
		if estadoDeId(t, m, id).State == api.StateFailed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state=%s tras matar el VMM readoptado; quería failed", estadoDeId(t, m, id).State)
}

// vigilarSalida sobre un proceso que ya no existe avisa igual, y nunca dentro
// de la llamada (se usa con m.mu tomado).
func TestVigilarSalidaDeUnProcesoQueYaNoExiste(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	avisado := make(chan struct{})
	fuera := make(chan struct{}) // se cierra cuando vuelve vigilarSalida
	if !vigilarSalida(pid, func() {
		<-fuera // si fn corriera dentro de la llamada, esto la colgaría
		close(avisado)
	}) {
		t.Skip("esta plataforma no sabe vigilar procesos ajenos")
	}
	close(fuera)
	select {
	case <-avisado:
	case <-time.After(3 * time.Second):
		t.Fatal("no avisó de un proceso que ya había terminado")
	}
}

// El arreglo 2: start sobre una running cuyo VMM murió sin que nadie lo
// viera no contesta "arrancada" con la máquina tal cual: la apunta failed y
// la arranca en frío de verdad (aquí falla al lanzar el VMM, que no hay, y
// queda parada: es la prueba de que lo intentó).
func TestStartDeUnaRunningMuertaLaArrancaDeVerdad(t *testing.T) {
	m := newTestManager(t)
	mc, _ := paradaParaStart(t, m, "5a5a0000000000b1")
	m.mu.Lock()
	m.byID[mc.ID].State, m.byID[mc.ID].PID = api.StateRunning, 999999
	m.mu.Unlock()
	_, err := m.Start(context.Background(), mc.ID, nil)
	if err == nil || !strings.Contains(err.Error(), "launching") {
		t.Fatalf("Start de una running muerta = %v; quería que intentara arrancarla", err)
	}
	if got := vivaDe(t, m, mc.ID).State; got != api.StateStopped {
		t.Fatalf("estado = %s; un arranque que falla la deja parada", got)
	}
}

// Y una running viva de verdad se devuelve tal cual, sin arrancar nada.
func TestStartDeUnaRunningVivaLaDevuelve(t *testing.T) {
	m := newTestManager(t)
	mc, _ := paradaParaStart(t, m, "5a5a0000000000b2")
	cmd := vmmQueSeVe(t, m, mc.ID)
	m.mu.Lock()
	m.byID[mc.ID].State, m.byID[mc.ID].PID = api.StateRunning, cmd.Process.Pid
	m.mu.Unlock()
	out, err := m.Start(context.Background(), mc.ID, nil)
	if err != nil || out.State != api.StateRunning || out.PID != cmd.Process.Pid {
		t.Fatalf("Start de una que corre = %+v, %v; quería devolverla", out, err)
	}
}

// El arreglo 3: una failed con su disco se arranca en frío como una parada;
// sin disco, o con un overlay desechable que no se asentó, no.
func TestStartDeUnaFailed(t *testing.T) {
	m := newTestManager(t)
	mc, _ := paradaParaStart(t, m, "5a5a0000000000b3")
	ponerFailed := func(motivo string) {
		ahora := time.Now()
		m.mu.Lock()
		m.byID[mc.ID].State, m.byID[mc.ID].LastErr, m.byID[mc.ID].FailedAt = api.StateFailed, motivo, &ahora
		m.mu.Unlock()
	}

	ponerFailed(errProcesoDesaparecido)
	// Un volcado viejo que deja de valer al arrancar en frío.
	dir := m.dir(mc.ID)
	for _, f := range []string{"snap.file", "mem.file"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := m.Start(context.Background(), mc.ID, nil)
	if err == nil || !strings.Contains(err.Error(), "launching") {
		t.Fatalf("Start de una failed con disco = %v; quería que intentara arrancarla", err)
	}
	if got := vivaDe(t, m, mc.ID); got.State != api.StateStopped || got.FailedAt != nil {
		t.Fatalf("estado = %s failedAt=%v; quería parada y sin fecha de fallo", got.State, got.FailedAt)
	}
	if existe(filepath.Join(dir, "mem.file")) {
		t.Fatal("el volcado de la failed sigue ahí tras arrancarla en frío")
	}

	ponerFailed(errOverlaySinAsentar)
	if _, err := m.Start(context.Background(), mc.ID, nil); err == nil || !errors.Is(err, ErrWrongState) {
		t.Fatalf("Start de una failed con el overlay sin asentar = %v; quería rechazo", err)
	}

	ponerFailed(errProcesoDesaparecido)
	if err := os.Remove(filepath.Join(dir, "overlay.ext4")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(context.Background(), mc.ID, nil); err == nil || !strings.Contains(err.Error(), "disk is gone") {
		t.Fatalf("Start de una failed sin disco = %v; quería 'disk is gone'", err)
	}
	if got := vivaDe(t, m, mc.ID).State; got != api.StateFailed {
		t.Fatalf("estado = %s; sin disco tiene que seguir failed", got)
	}
}

// thaw de una failed: solo con su volcado entero y un disco que no cambió
// después de él.
func TestThawDeUnaFailed(t *testing.T) {
	m := newTestManager(t)
	mc, _ := paradaParaStart(t, m, "5a5a0000000000b4")
	ahora := time.Now()
	m.mu.Lock()
	m.byID[mc.ID].State, m.byID[mc.ID].LastErr, m.byID[mc.ID].FailedAt = api.StateFailed, errProcesoDesaparecido, &ahora
	m.mu.Unlock()
	dir := m.dir(mc.ID)

	// Sin volcado: a start.
	if _, err := m.Thaw(context.Background(), mc.ID); err == nil || !strings.Contains(err.Error(), "kling start") {
		t.Fatalf("Thaw de una failed sin volcado = %v; quería que remitiera a kling start", err)
	}

	for _, f := range []string{"snap.file", "mem.file"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("volcado"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, marcaEnCurso), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sellarVolcado(dir, "", "", "", false); err != nil {
		t.Fatal(err)
	}
	// El disco, escrito después del volcado: cargarlo lo corrompería.
	despues := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(dir, "overlay.ext4"), despues, despues); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Thaw(context.Background(), mc.ID); err == nil || !strings.Contains(err.Error(), "written after") {
		t.Fatalf("Thaw con el disco más nuevo que el volcado = %v; quería rechazo", err)
	}

	// Disco anterior al volcado: pasa la puerta y sigue como un thaw (aquí
	// acaba fallando más adelante, sin VMM, y la máquina sigue failed).
	antes := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "overlay.ext4"), antes, antes); err != nil {
		t.Fatal(err)
	}
	_, err := m.Thaw(context.Background(), mc.ID)
	if err != nil && (errors.Is(err, ErrWrongState) || strings.Contains(err.Error(), "is failed")) {
		t.Fatalf("Thaw de una failed con volcado bueno = %v; no tenía que rechazarla", err)
	}
	if err == nil || !strings.Contains(err.Error(), "launching") {
		t.Fatalf("Thaw de una failed con volcado bueno = %v; quería que llegara a lanzar el VMM", err)
	}
	if got := vivaDe(t, m, mc.ID).State; got != api.StateFailed {
		t.Fatalf("estado = %s tras un thaw fallido; quería que siguiera failed", got)
	}
}

// Una running muerta que se descongela: thaw la ve muerta y la trata como
// failed, en vez de contestar "running" con la máquina tal cual.
func TestThawDeUnaRunningMuertaNoLaDevuelve(t *testing.T) {
	m := newTestManager(t)
	mc, _ := paradaParaStart(t, m, "5a5a0000000000b5")
	m.mu.Lock()
	m.byID[mc.ID].State, m.byID[mc.ID].PID = api.StateRunning, 999999
	m.mu.Unlock()
	if _, err := m.Thaw(context.Background(), mc.ID); err == nil || !strings.Contains(err.Error(), "kling start") {
		t.Fatalf("Thaw de una running muerta = %v; quería el rechazo de una failed sin volcado", err)
	}
	if got := vivaDe(t, m, mc.ID); got.State != api.StateFailed || got.LastErr != errProcesoDesaparecido {
		t.Fatalf("estado = %s (%q); quería failed por el proceso desaparecido", got.State, got.LastErr)
	}
}
