package machine

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// plantillaSinProceso es plantillaParaCommit sin el proceso falso de /proc:
// una running con su socket de Firecracker falso y su overlay. Basta para los
// caminos de Commit que no matan el VMM, y corre también en macOS.
func plantillaSinProceso(t *testing.T, m *Manager, id string) *fcFalso {
	t.Helper()
	m.bus = events.New()
	m.priv = &Privileges{}
	falso := nuevoFcFalso(t)
	m.addForTest(id)
	m.mu.Lock()
	m.socket[id] = falso.Sock
	m.mu.Unlock()
	for _, d := range []string{m.dir(id), filepath.Join(m.root, "images")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(m.dir(id), "overlay.ext4"), make([]byte, 64<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	return falso
}

// anteriores lista los dorados viejos apartados por un -replace.
func anteriores(t *testing.T, m *Manager) []string {
	t.Helper()
	es, _ := filepath.Glob(filepath.Join(m.root, "snapshots", ".*"+sufijoAnterior+"*"))
	return es
}

// commit -replace con un volcado que falla (disco lleno, por ejemplo) deja el
// dorado viejo EXACTAMENTE como estaba: antes se borraba primero y el fallo
// dejaba sin ninguno de los dos.
func TestCommitReplaceFallidoConservaElDoradoViejo(t *testing.T) {
	m := newTestManager(t)
	id := "c0aa170000000011"
	falso := plantillaSinProceso(t, m, id)
	dir := escribirSnapshot(t, m, "dorado", api.Snapshot{Image: "vieja"})
	if err := os.WriteFile(filepath.Join(dir, "mem.file"), []byte("memoria vieja"), 0o644); err != nil {
		t.Fatal(err)
	}
	falso.fallar(http.MethodPut, "/snapshot/create", http.StatusBadRequest, "No space left on device")

	if _, err := m.Commit(context.Background(), id, "dorado", true); err == nil {
		t.Fatal("el commit con el volcado roto no falló")
	}
	s, err := m.loadSnapshot("dorado")
	if err != nil || s.Image != "vieja" {
		t.Fatalf("el dorado viejo no sobrevivió al -replace fallido: %+v, %v", s, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "mem.file")); string(b) != "memoria vieja" {
		t.Errorf("mem.file del viejo = %q", b)
	}
	if a := anteriores(t, m); len(a) != 0 {
		t.Errorf("quedaron apartados: %v", a)
	}
}

// Con éxito, el nuevo ocupa el nombre y el viejo desaparece.
func TestCommitReplaceCompletoSustituyeAlViejo(t *testing.T) {
	if _, err := exec.LookPath("fallocate"); err != nil {
		t.Skip("sin fallocate no se puede perforar el volcado")
	}
	m := newTestManager(t)
	id := "c0aa170000000012"
	falso := plantillaSinProceso(t, m, id)
	if err := os.WriteFile(m.KernelPath(), []byte("vmlinux de prueba"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := escribirSnapshot(t, m, "dorado", api.Snapshot{Image: "vieja"})
	falso.enGancho(func(metodo, ruta string) {
		if ruta == "/snapshot/create" {
			_ = os.WriteFile(filepath.Join(dir, "snap.file"), []byte("estado"), 0o644)
			_ = os.WriteFile(filepath.Join(dir, "mem.file"), make([]byte, 1<<20), 0o644)
		}
	})
	if _, err := m.Commit(context.Background(), id, "dorado", true); err != nil {
		t.Fatal(err)
	}
	if s, err := m.loadSnapshot("dorado"); err != nil || s.Image == "vieja" {
		t.Fatalf("tras el -replace sigue el viejo: %+v, %v", s, err)
	}
	if a := anteriores(t, m); len(a) != 0 {
		t.Errorf("el viejo quedó apartado: %v", a)
	}
}

// Si el daemon murió a mitad de un -replace, al arrancar vuelve el viejo (el
// nuevo no llegó a su meta.json) o se retira (sí llegó).
func TestRecuperarReemplazosAlArrancar(t *testing.T) {
	m := newTestManager(t)
	escribirSnapshot(t, m, "a-medias", api.Snapshot{Image: "vieja"})
	if _, err := m.apartarParaReemplazo("a-medias"); err != nil {
		t.Fatal(err)
	}
	// Lo que dejó el commit interrumpido: un directorio sin meta.json.
	if err := os.MkdirAll(m.snapDir("a-medias"), 0o755); err != nil {
		t.Fatal(err)
	}
	escribirSnapshot(t, m, "acabado", api.Snapshot{Image: "vieja"})
	if _, err := m.apartarParaReemplazo("acabado"); err != nil {
		t.Fatal(err)
	}
	escribirSnapshot(t, m, "acabado", api.Snapshot{Image: "nueva"})

	m.recuperarReemplazos()

	if s, err := m.loadSnapshot("a-medias"); err != nil || s.Image != "vieja" {
		t.Errorf("no volvió el viejo de un -replace a medias: %+v, %v", s, err)
	}
	if s, err := m.loadSnapshot("acabado"); err != nil || s.Image != "nueva" {
		t.Errorf("un -replace terminado perdió el nuevo: %+v, %v", s, err)
	}
	if a := anteriores(t, m); len(a) != 0 {
		t.Errorf("quedaron apartados: %v", a)
	}
	for _, s := range m.Snapshots() {
		if strings.HasPrefix(s.Name, ".") {
			t.Errorf("Snapshots lista un apartado: %s", s.Name)
		}
	}
}
