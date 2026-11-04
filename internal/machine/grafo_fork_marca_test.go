package machine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// El volcado de un nodo en un GraphFork deja la marca de fork ANTES del
// meta.json, como el de un fork de máquina (TestCommitDeForkMarcaAntesDelMeta):
// con la marca después, un corte entre las dos dejaba una plantilla válida que
// no recogían ni el barrido de restos ni barrerForks.
func TestGraphForkMarcaAntesDelMeta(t *testing.T) {
	if _, err := exec.LookPath("fallocate"); err != nil && !volcadoEnLaMaquina {
		t.Skip("sin fallocate no se puede perforar el volcado")
	}
	m := newTestManager(t)
	const id = "c0aa170000000104"
	falso := plantillaSinProceso(t, m, id)
	if err := os.WriteFile(m.KernelPath(), []byte("vmlinux de prueba"), 0o644); err != nil {
		t.Fatal(err)
	}
	// commitPausada pide la máquina ya pausada: así la deja snapshotConsistente.
	m.mu.Lock()
	m.byID[id].State = api.StatePaused
	m.mu.Unlock()

	volcar := func(ctx context.Context, name string) string {
		t.Helper()
		dir := m.snapDir(name)
		snapTmp, memTmp := filepath.Join(dir, "snap.file"), filepath.Join(dir, "mem.file")
		if volcadoEnLaMaquina {
			snapTmp, memTmp = filepath.Join(m.dir(id), volcadoTmpSnap), filepath.Join(m.dir(id), volcadoTmpMem)
		}
		falso.enGancho(func(metodo, ruta string) {
			if ruta == "/snapshot/create" {
				_ = os.WriteFile(snapTmp, []byte("estado"), 0o644)
				_ = os.WriteFile(memTmp, make([]byte, 1<<20), 0o644)
			}
		})
		if err := volcarNodoPausado(ctx, m, id, name); err != nil {
			t.Fatalf("volcado de %s: %v", name, err)
		}
		return dir
	}

	dir := volcar(conCommitDeFork(context.Background()), "gfork-c0aa17000000-api-12345678")
	marca, err := os.Stat(filepath.Join(dir, forkMarca))
	if err != nil {
		t.Fatalf("el volcado de un GraphFork no dejó la marca: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, forkMarca)); string(b) != id+"\n" {
		t.Errorf("la marca dice %q, quería el id del nodo original", b)
	}
	meta, err := os.Stat(filepath.Join(dir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if marca.ModTime().After(meta.ModTime()) {
		t.Errorf("la marca (%s) es posterior al meta.json (%s)",
			marca.ModTime().Format(time.RFC3339Nano), meta.ModTime().Format(time.RFC3339Nano))
	}

	// Un GraphSnapshot (sin conCommitDeFork) no es un fork: sin marca.
	dir = volcar(context.Background(), "tienda-api-1")
	if _, err := os.Stat(filepath.Join(dir, forkMarca)); !os.IsNotExist(err) {
		t.Errorf("el volcado de un GraphSnapshot quedó marcado como fork: %v", err)
	}
}
