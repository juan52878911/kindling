package daemon

import (
	"bufio"
	"context"
	"io"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// tubo es una conexión de mentira con dos direcciones independientes.
type tubo struct {
	io.Reader
	io.Writer
}

func (t tubo) Close() error { return nil }

// relé montado sobre tuberías: cliente ↔ daemon ↔ invitado, sin microVM.
func montarRele(t *testing.T) (cliente *bufio.Reader, aDaemon *io.PipeWriter, invitado *bufio.Reader, aCliente *io.PipeWriter, delInvitado *io.PipeWriter, fin chan struct{}) {
	t.Helper()
	clienteR, clienteW := io.Pipe() // daemon → cliente
	daemonR, daemonW := io.Pipe()   // cliente → daemon
	invR, invW := io.Pipe()         // daemon → invitado
	respR, respW := io.Pipe()       // invitado → daemon

	fin = make(chan struct{})
	go func() {
		defer close(fin)
		relayShell(context.Background(), daemonR, clienteW, tubo{Reader: respR, Writer: invW})
	}()
	t.Cleanup(func() {
		daemonW.Close()
		respW.Close()
	})
	return bufio.NewReader(clienteR), daemonW, bufio.NewReader(invR), clienteW, respW, fin
}

func leerTrama(t *testing.T, br *bufio.Reader) (byte, []byte) {
	t.Helper()
	type r struct {
		t byte
		p []byte
	}
	ch := make(chan r, 1)
	go func() {
		tipo, p, err := api.ReadFrame(br)
		if err != nil {
			ch <- r{255, nil}
			return
		}
		ch <- r{tipo, p}
	}()
	select {
	case got := <-ch:
		return got.t, got.p
	case <-time.After(5 * time.Second):
		t.Fatal("no llegó ninguna trama")
		return 0, nil
	}
}

// Lo que el cliente manda llega al invitado, y lo que el invitado responde
// vuelve: el caso normal.
func TestReleShellPasaTramasEnLosDosSentidos(t *testing.T) {
	cliente, aDaemon, invitado, _, delInvitado, _ := montarRele(t)

	go api.WriteFrame(aDaemon, api.ShellData, []byte("ls\n"))
	tipo, p := leerTrama(t, invitado)
	if tipo != api.ShellData || string(p) != "ls\n" {
		t.Fatalf("el invitado recibió (%d, %q)", tipo, p)
	}

	go api.WriteFrame(delInvitado, api.ShellData, []byte("bin etc\n"))
	tipo, p = leerTrama(t, cliente)
	if tipo != api.ShellData || string(p) != "bin etc\n" {
		t.Fatalf("el cliente recibió (%d, %q)", tipo, p)
	}
}

// El invitado es hostil: no puede mandarle al cliente tramas que solo tienen
// sentido hacia dentro (redimensionar su terminal, señales a su proceso).
func TestReleShellNoDejaQueElInvitadoMandeLoQueQuiera(t *testing.T) {
	cliente, _, _, _, delInvitado, fin := montarRele(t)

	go api.WriteFrame(delInvitado, api.ShellResize, api.ResizePayload(10, 10))
	tipo, p := leerTrama(t, cliente)
	if tipo != api.ShellError {
		t.Fatalf("(%d, %q): un resize del invitado tenía que cortar la sesión", tipo, p)
	}
	select {
	case <-fin:
	case <-time.After(5 * time.Second):
		t.Fatal("la sesión siguió abierta")
	}
}

// Y al revés: un cliente que manda un "exit" no puede hacer creer al invitado
// que la sesión terminó. Se ignora, sin tumbar la sesión.
func TestReleShellIgnoraLoQueElClienteNoDebeMandar(t *testing.T) {
	_, aDaemon, invitado, _, _, _ := montarRele(t)

	go func() {
		api.WriteFrame(aDaemon, api.ShellExit, api.ExitPayload(0))
		api.WriteFrame(aDaemon, api.ShellData, []byte("sigo aquí"))
	}()
	tipo, p := leerTrama(t, invitado)
	if tipo != api.ShellData || string(p) != "sigo aquí" {
		t.Fatalf("el invitado recibió (%d, %q); el exit del cliente debía descartarse sin más", tipo, p)
	}
}

// Cuando el invitado cuelga sin decir nada, el cliente se entera: una sesión que
// se queda muda para siempre es lo peor que puede pasar aquí.
func TestReleShellAvisaSiElInvitadoDesaparece(t *testing.T) {
	cliente, _, _, _, delInvitado, _ := montarRele(t)

	delInvitado.Close()
	tipo, p := leerTrama(t, cliente)
	if tipo != api.ShellError || len(p) == 0 {
		t.Fatalf("(%d, %q), quería un error explicando que el invitado dejó de contestar", tipo, p)
	}
}

// El final del invitado se reenvía tal cual y cierra la sesión.
func TestReleShellReenviaElFinal(t *testing.T) {
	cliente, _, _, _, delInvitado, fin := montarRele(t)

	go api.WriteFrame(delInvitado, api.ShellExit, api.ExitPayload(9))
	tipo, p := leerTrama(t, cliente)
	if tipo != api.ShellExit {
		t.Fatalf("tipo %d, want exit", tipo)
	}
	if code, _ := api.ParseExit(p); code != 9 {
		t.Fatalf("código %d, want 9", code)
	}
	select {
	case <-fin:
	case <-time.After(5 * time.Second):
		t.Fatal("la sesión no se cerró tras el final")
	}
}
