//go:build linux

package guest

import (
	"net"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Quien abrió la shell y deja de leer no se queda con la plaza: la escritura
// vence su plazo, el maestro se cierra (SIGHUP) y runShell vuelve.
func TestShellCortaAQuienNoLee(t *testing.T) {
	if err := ptySupported(); err != nil {
		t.Skipf("sin pseudoterminales aquí: %v", err)
	}
	antes := shellEscritura
	shellEscritura = 100 * time.Millisecond
	t.Cleanup(func() { shellEscritura = antes })

	agente, llamante := net.Pipe() // nadie lee de llamante
	t.Cleanup(func() { agente.Close(); llamante.Close() })

	fin := make(chan struct{})
	go func() {
		defer close(fin)
		runShell(nil, api.ShellRequest{Cmd: []string{"/bin/sh", "-c", "yes"}}, agente, agente)
	}()
	select {
	case <-fin:
	case <-time.After(10 * time.Second):
		t.Fatal("la shell sigue viva esperando a quien no lee")
	}
}
