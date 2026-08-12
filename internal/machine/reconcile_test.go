package machine

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
