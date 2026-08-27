package machine

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/api"
	"github.com/juan52878911/kindling/internal/events"
)

// liveVMs es la pieza sobre la que se apoya todo el reconcile nuevo: si no ve
// un firecracker vivo, reconcile le quita la red y el cgroup a una máquina que
// está funcionando.
//
// Se ejercita con un proceso real cuya línea de comandos imita la de
// firecracker, porque lo que se prueba es precisamente el parseo de /proc.
func TestLiveVMsEncuentraLaMaquinaPorSuSocket(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("liveVMs lee /proc; solo aplica en Linux")
	}
	m := newTestManager(t)
	id := "abcdef0123456789"
	sock := filepath.Join(m.dir(id), "fc.sock")
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sleep", "30", "--api-sock", sock)
	if err := cmd.Start(); err != nil {
		t.Skipf("no pude lanzar el proceso de prueba: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	live := m.liveVMs()
	got, ok := live[id]
	if !ok {
		t.Fatalf("no encontró la máquina viva; vio: %v", live)
	}
	if got != cmd.Process.Pid {
		t.Errorf("pid = %d, want %d", got, cmd.Process.Pid)
	}
}

// Un proceso ajeno que mencione una ruta parecida no debe confundirse con una
// microVM: reconcile actúa sobre lo que aquí se devuelva.
func TestLiveVMsNoSeConfundeConRutasAjenas(t *testing.T) {
	m := newTestManager(t)
	live := m.liveVMs()
	for id := range live {
		if strings.Contains(id, "/") {
			t.Errorf("devolvió un id con barras: %q", id)
		}
	}
}

// El barrido de huérfanos no puede llevarse por delante el directorio de una
// máquina que se está CONSTRUYENDO.
//
// Reproduce la carrera real: runFrom crea el directorio, copia ~100 MiB de
// overlay dorado, resuelve volúmenes y monta la red, y solo entonces registra la
// máquina en byID. En esa ventana —cientos de milisegundos— el directorio no era
// de "ninguna máquina conocida", y el GC de disco, que barre cada 10 s en cuanto
// el anfitrión va justo de espacio, lo borraba. El caller veía un error sobre un
// firecracker.log que no existe, que no dice nada de la causa.
func TestSweepNoBorraElDirectorioDeUnaMaquinaEnConstruccion(t *testing.T) {
	m := newTestManager(t)
	id := "0123456789abcdef0123456789abcdef"

	// Barrido agresivo en paralelo, como el del disco lleno.
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.mu.Lock()
			m.sweepMachineDirs()
			m.mu.Unlock()
		}
	}()
	defer func() { close(stop); <-done }()

	dir, unreserve, err := m.makeMachineDir(id)
	if err != nil {
		t.Fatal(err)
	}
	defer unreserve()

	// El llenado lento del directorio: si el barrido se lo lleva, esto falla con
	// un ENOENT, que es exactamente lo que se veía en el laboratorio.
	for i := 0; i < 50; i++ {
		if err := os.WriteFile(filepath.Join(dir, "overlay.ext4"), []byte("golden"), 0o644); err != nil {
			t.Fatalf("el barrido borró el directorio a medio construir: %v", err)
		}
		time.Sleep(time.Millisecond)
	}

	// El registro en byID llega al final, como en producción, y a partir de ahí
	// la máquina se protege sola.
	m.addForTest(id)

	// El síntoma que se reportó, comprobado tal cual.
	if _, err := os.Create(filepath.Join(dir, "firecracker.log")); err != nil {
		t.Fatalf("open firecracker.log: %v", err)
	}
}

// La reserva tiene que valer POR SÍ SOLA, sin apoyarse en la edad del
// directorio: una copia de overlay sobre un anfitrión cargado puede tardar más
// que el margen de cortesía, y entonces solo queda ella.
func TestSweepRespetaLaReservaAunqueElDirectorioSeaViejo(t *testing.T) {
	m := newTestManager(t)
	id := "aaaabbbbccccdddd"

	dir, unreserve, err := m.makeMachineDir(id)
	if err != nil {
		t.Fatal(err)
	}
	defer unreserve()
	envejecer(t, dir)

	m.mu.Lock()
	m.sweepMachineDirs()
	m.mu.Unlock()

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("borró un directorio reservado: %v", err)
	}
}

// El margen de cortesía es el cinturón sobre los tirantes: cubre a cualquier
// camino futuro que cree un directorio antes de registrar su id.
func TestSweepPerdonaLosDirectoriosRecienTocados(t *testing.T) {
	m := newTestManager(t)
	dir := m.dir("eeeeffff00001111")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	m.sweepMachineDirs()
	m.mu.Unlock()

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("borró un directorio recién creado sin darle margen: %v", err)
	}
}

// Que el barrido SIGA recogiendo huérfanos de verdad lo cubre
// TestSeBorranLosDirectoriosHuerfanos, en gc_test.go.

// envejecer retrasa la fecha del directorio más allá del margen de cortesía, que
// es la única forma de probar el barrido sin dormir dos minutos.
func envejecer(t *testing.T, dir string) {
	t.Helper()
	viejo := time.Now().Add(-2 * dirGrace)
	if err := os.Chtimes(dir, viejo, viejo); err != nil {
		t.Fatal(err)
	}
}

// nsName y cgName tienen que coincidir con lo que generan knet.Plan y el gestor
// de cgroups, o reconcile protegería nombres que no existen y borraría los que
// sí. Es el tipo de duplicación que se rompe sola con el tiempo.
func TestNombresDerivadosCoincidenConLosReales(t *testing.T) {
	ids := []string{"abcdef0123456789", "0123456789abcdef0123", "corto"}
	for _, id := range ids {
		if got, want := nsName(id), "kl-"+shortID(id); got != want {
			t.Errorf("nsName(%q) = %q, want %q", id, got, want)
		}
		// El cgroup se nombra "kl-"+id[:8] en cgroup.go y en sweep().
		if got, want := cgName(id), "kl-"+shortID(id); got != want {
			t.Errorf("cgName(%q) = %q, want %q", id, got, want)
		}
	}
}

// hasSnapshot decide si una máquina que perdió su proceso queda warm (con el
// snapshot recuperable) o stopped. Equivocarse hacia stopped deja el snapshot
// varado, porque Thaw se niega a descongelar lo que no esté warm.
func TestHasSnapshotExigeLosDosFicheros(t *testing.T) {
	m := newTestManager(t)
	id := "aaaa1111bbbb2222"
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if m.hasSnapshot(id) {
		t.Error("sin ficheros no debería haber snapshot")
	}
	if err := os.WriteFile(filepath.Join(dir, "snap.file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m.hasSnapshot(id) {
		t.Error("con solo snap.file el congelado está incompleto")
	}
	if err := os.WriteFile(filepath.Join(dir, "mem.file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !m.hasSnapshot(id) {
		t.Error("con snap.file y mem.file sí hay snapshot")
	}
}

// El segador de TTL tiene que distinguir "no pude congelarla AHORA" de "no voy
// a poder congelarla NUNCA". Lo segundo es un socket que ya no existe o nadie
// escuchando: reintentarlo cada 10 segundos es lo que produjo 260 horas de
// fallos idénticos sobre la misma máquina.
func TestElFalloDeSocketDesaparecidoEsEstructural(t *testing.T) {
	// Así llega de verdad: el dial del cliente HTTP envuelve el errno del
	// sistema en un net.OpError, como en el fallo observado
	// ("dial unix .../fc.sock: connect: no such file or directory").
	enoent := fmt.Errorf("Patch \"http://localhost/vm\": %w",
		&net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.ENOENT)})
	if !freezeErrIsStructural(enoent) {
		t.Error("un ENOENT al conectar al socket es irrecuperable y no lo reconoció")
	}
	refused := fmt.Errorf("Patch \"http://localhost/vm\": %w",
		&net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)})
	if !freezeErrIsStructural(refused) {
		t.Error("un ECONNREFUSED (socket sin nadie detrás) es irrecuperable y no lo reconoció")
	}

	// Lo transitorio se queda transitorio: un timeout o un error cualquiera
	// merecen reintento, y tratarlos de estructurales mataría máquinas sanas.
	if freezeErrIsStructural(errors.New("context deadline exceeded")) {
		t.Error("trató un timeout como estructural")
	}
	if freezeErrIsStructural(fmt.Errorf("firecracker PATCH /vm: 400 Bad Request: some transient thing")) {
		t.Error("trató un error genérico como estructural")
	}
	if freezeErrIsStructural(nil) {
		t.Error("nil no es un fallo de nada")
	}
}

// Y aunque el fallo no se sepa clasificar, el segador se rinde tras un número
// finito de intentos seguidos: la máquina pasa a failed (terminal) y no se
// vuelve a intentar. Un fallo "transitorio" que dura horas no es transitorio.
func TestElSegadorSeRindeTrasDemasiadosFallosSeguidos(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "feedfacefeedface"
	now := time.Now()
	m.mu.Lock()
	m.byID[id] = &api.Machine{ID: id, Name: "tozuda", State: api.StateRunning,
		StartedAt: &now, TTLSeconds: 1}
	m.mu.Unlock()

	// Un fallo que no se sabe clasificar, repetido: los primeros se toleran…
	for i := 0; i < maxFreezeFailures-1; i++ {
		if n := m.noteFreezeFailure(id); n >= maxFreezeFailures {
			t.Fatalf("se rindió en el intento %d, antes del tope %d", n, maxFreezeFailures)
		}
	}
	// …y el que llega al tope, no.
	if n := m.noteFreezeFailure(id); n < maxFreezeFailures {
		t.Fatalf("tras %d fallos seguidos el contador dice %d", maxFreezeFailures, n)
	}
	m.giveUpOn(id, errors.New("gave up for the test"))

	m.mu.RLock()
	mc := m.byID[id]
	m.mu.RUnlock()
	if mc.State != api.StateFailed {
		t.Fatalf("tras rendirse la máquina debe quedar failed (terminal), está %q", mc.State)
	}
	if mc.FailedAt == nil {
		t.Error("una failed sin FailedAt no se puede recoger después")
	}
	// Y el contador se limpió: si reviviera, empezaría de cero.
	m.mu.RLock()
	_, sigue := m.freezeFails[id]
	m.mu.RUnlock()
	if sigue {
		t.Error("el contador de fallos no se limpió al rendirse")
	}
}

// Un éxito (o que otro camino la congele) borra la cuenta: los fallos solo
// suman si son consecutivos, que es lo que separa una racha mala de una avería.
func TestUnExitoReiniciaElContadorDeFallos(t *testing.T) {
	m := newTestManager(t)
	id := "cafebabecafebabe"
	for i := 0; i < 5; i++ {
		m.noteFreezeFailure(id)
	}
	m.clearFreezeFailures(id)
	if n := m.noteFreezeFailure(id); n != 1 {
		t.Errorf("tras limpiar, el siguiente fallo debía ser el 1, fue el %d", n)
	}
}

// handleFreezeFailure no debe tocar una máquina que ya no está running: si otro
// camino la congeló o la paró mientras tanto, el "fallo" ya no significa nada.
func TestElSegadorNoTocaLoQueYaNoCorre(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "0123456789abcdef"
	m.mu.Lock()
	m.byID[id] = &api.Machine{ID: id, Name: "dormida", State: api.StateWarm}
	m.mu.Unlock()
	m.noteFreezeFailure(id)

	m.handleFreezeFailure(id, errors.New("whatever"))

	m.mu.RLock()
	estado := m.byID[id].State
	_, conCuenta := m.freezeFails[id]
	m.mu.RUnlock()
	if estado != api.StateWarm {
		t.Errorf("cambió el estado de una máquina que ya no corría: %q", estado)
	}
	if conCuenta {
		t.Error("no limpió el contador de una máquina que ya no corre")
	}
}
