package daemon

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Un cliente que deja de leer no retiene la sesión: la escritura vence su
// plazo y el relé acaba (quien lo llamó cierra entonces las dos puntas y la
// plaza del invitado queda libre).
func TestReleShellCortaAlClienteQueNoLee(t *testing.T) {
	antes := shellEscritura
	shellEscritura = 100 * time.Millisecond
	t.Cleanup(func() { shellEscritura = antes })

	daemonC, clienteC := net.Pipe() // net.Pipe: escribir espera a que el otro lea
	defer clienteC.Close()
	defer daemonC.Close()
	invR, invW := io.Pipe()
	respR, respW := io.Pipe()
	defer respW.Close()
	go io.Copy(io.Discard, invR)

	fin := make(chan struct{})
	go func() {
		defer close(fin)
		relayShell(context.Background(), daemonC, daemonC, tubo{Reader: respR, Writer: invW})
	}()
	// El invitado habla; el cliente nunca lee.
	go func() {
		for api.WriteFrame(respW, api.ShellData, []byte("salida\n")) == nil {
		}
	}()
	select {
	case <-fin:
	case <-time.After(5 * time.Second):
		t.Fatal("el relé sigue esperando a un cliente que no lee")
	}
}
