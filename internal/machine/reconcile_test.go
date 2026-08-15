package machine

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
