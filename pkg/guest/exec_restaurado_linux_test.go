//go:build linux

package guest

import (
	"io"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Una shell abierta al volcar el dorado también es de otra máquina: el
// /resync de una copia cierra su conexión.
func TestResyncCopiaCierraShells(t *testing.T) {
	falsos(t, nil, nil)
	_, br := abrirShell(t, api.ShellRequest{Cmd: []string{"sleep", "100"}})
	// Que la shell esté en marcha antes de cortar.
	time.Sleep(200 * time.Millisecond)
	if n := cortarExecs(); n != 1 {
		t.Fatalf("cortarExecs cortó %d, quería 1", n)
	}
	fin := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, br)
		close(fin)
	}()
	select {
	case <-fin:
	case <-time.After(5 * time.Second):
		t.Fatal("la conexión de la shell sigue abierta tras cortar")
	}
}
