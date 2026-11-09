package machine

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Un VMM readoptado que muere después de Close no puede escribir nada: el
// aviso de su salida (vigilarAdoptado → vmmTermino) llegaba asíncrono, la
// marcaba failed y, al soltar el cerrojo con el Manager ya cerrado
// (trasCierre), reescribía state.json. En las pruebas eso chocaba con el
// RemoveAll del TempDir ("directory not empty", CI de main tras #240); en el
// daemon, es escribir tras haber dicho que ya no se escribe.
func TestVMMQueMuereTrasCerrarNoEscribe(t *testing.T) {
	m := newTestManager(t)
	id := "c1e2000000000001"
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd, fin := lanzarProcesoFalso(t, argSocketVMM, filepath.Join(m.dir(id), "fc.sock"))
	corriendoCon(m, id, cmd.Process.Pid)
	if !vigilarSalida(cmd.Process.Pid, func() { m.vmmTermino(id, detalleAdoptado) }) {
		t.Skip("aquí no se puede vigilar la salida de un proceso")
	}
	m.Close()
	antes, err := os.ReadFile(m.statePath())
	if err != nil {
		t.Fatal(err)
	}

	_ = cmd.Process.Kill()
	<-fin
	time.Sleep(300 * time.Millisecond) // lo que tarde el aviso en llegar
	despues, _ := os.ReadFile(m.statePath())
	if !bytes.Equal(antes, despues) {
		t.Fatalf("state.json cambió tras Close:\nantes:\n%s\ndespués:\n%s", antes, despues)
	}
	m.mu.RLock()
	estado := m.byID[id].State
	m.mu.RUnlock()
	if estado != api.StateRunning {
		t.Fatalf("estado = %s tras Close; quería que nadie la tocase", estado)
	}
}
