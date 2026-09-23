//go:build linux

package guest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// abrirShell levanta el agente en un servidor de verdad (httptest, no
// ResponseRecorder: el recorder no implementa http.Hijacker y esta ruta vive de
// secuestrar la conexión) y devuelve la sesión ya negociada.
func abrirShell(t *testing.T, req api.ShellRequest) (io.ReadWriteCloser, *bufio.Reader) {
	t.Helper()
	if err := ptySupported(); err != nil {
		t.Skipf("sin pseudoterminales aquí: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/exec/pty", ShellHandler(nil))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(req)
	hreq, err := http.NewRequest(http.MethodPost, srv.URL+"/exec/pty", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	hreq.Header.Set("Connection", "Upgrade")
	hreq.Header.Set("Upgrade", api.ShellProto)
	resp, err := (&http.Client{}).Do(hreq)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		t.Fatal("el cuerpo no es escribible: no hubo upgrade")
	}
	t.Cleanup(func() { rwc.Close() })
	return rwc, bufio.NewReader(rwc)
}

// leerHasta acumula la salida hasta que llega el final o se agota el plazo.
func leerHasta(t *testing.T, br *bufio.Reader, hasta time.Duration) (string, int32, error) {
	t.Helper()
	type res struct {
		out  string
		code int32
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		var sb strings.Builder
		for {
			tipo, p, err := api.ReadFrame(br)
			if err != nil {
				ch <- res{sb.String(), -1, err}
				return
			}
			switch tipo {
			case api.ShellData:
				sb.Write(p)
			case api.ShellExit:
				code, _ := api.ParseExit(p)
				ch <- res{sb.String(), code, nil}
				return
			case api.ShellError:
				ch <- res{sb.String(), -1, errors.New("the guest sent an error: " + string(p))}
				return
			}
		}
	}()
	select {
	case r := <-ch:
		return r.out, r.code, r.err
	case <-time.After(hasta):
		t.Fatalf("la sesión no terminó en %s", hasta)
		return "", 0, nil
	}
}

// Lo que distingue una shell de un exec: hay un terminal de verdad dentro, con
// su tamaño, y el proceso lo ve.
func TestShellTieneTerminalConSuTamano(t *testing.T) {
	rwc, br := abrirShell(t, api.ShellRequest{
		Cmd: []string{"/bin/sh", "-c", "tty; stty size; exit 7"}, Rows: 30, Cols: 100,
	})
	defer rwc.Close()

	out, code, err := leerHasta(t, br, 20*time.Second)
	if err != nil {
		t.Fatalf("%v (salida: %q)", err, out)
	}
	if code != 7 {
		t.Errorf("código = %d, want 7", code)
	}
	if !strings.Contains(out, "/dev/pts/") {
		t.Errorf("tty no devolvió un pseudoterminal: %q", out)
	}
	if !strings.Contains(out, "30 100") {
		t.Errorf("stty size = %q, quería 30 100", out)
	}
}

// La entrada llega según se teclea, no entera al final: es la mitad que /exec no
// puede dar.
func TestShellLeeLaEntradaMientrasCorre(t *testing.T) {
	rwc, br := abrirShell(t, api.ShellRequest{
		Cmd: []string{"/bin/sh", "-c", "read linea; echo \"[$linea]\"; exit 0"},
	})
	defer rwc.Close()

	time.Sleep(300 * time.Millisecond) // que la shell llegue al read
	if err := api.WriteFrame(rwc, api.ShellData, []byte("hola\n")); err != nil {
		t.Fatal(err)
	}
	out, code, err := leerHasta(t, br, 20*time.Second)
	if err != nil {
		t.Fatalf("%v (salida: %q)", err, out)
	}
	if code != 0 || !strings.Contains(out, "[hola]") {
		t.Errorf("salida %q, código %d", out, code)
	}
}

// Ctrl-C viaja como byte y es el PTY quien lo convierte en SIGINT sobre el
// proceso en primer plano. Si esto se rompe, Ctrl-C mataría la sesión entera en
// vez de lo que corre dentro.
func TestShellCtrlCMataLoQueCorreDentro(t *testing.T) {
	rwc, br := abrirShell(t, api.ShellRequest{
		Cmd: []string{"/bin/sh", "-c", "sleep 60"},
	})
	defer rwc.Close()

	time.Sleep(500 * time.Millisecond)
	if err := api.WriteFrame(rwc, api.ShellData, []byte{0x03}); err != nil {
		t.Fatal(err)
	}
	_, code, err := leerHasta(t, br, 20*time.Second)
	if err != nil {
		t.Fatalf("no terminó tras Ctrl-C: %v", err)
	}
	if code == 0 {
		t.Errorf("código = 0; un sleep interrumpido no sale con 0")
	}
}

// Redimensionar tiene que llegar al programa de dentro.
func TestShellRedimensiona(t *testing.T) {
	rwc, br := abrirShell(t, api.ShellRequest{
		Cmd: []string{"/bin/sh", "-c", "read x; stty size; exit 0"}, Rows: 24, Cols: 80,
	})
	defer rwc.Close()

	time.Sleep(300 * time.Millisecond)
	if err := api.WriteFrame(rwc, api.ShellResize, api.ResizePayload(50, 200)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	_ = api.WriteFrame(rwc, api.ShellData, []byte("\n"))

	out, _, err := leerHasta(t, br, 20*time.Second)
	if err != nil {
		t.Fatalf("%v (salida %q)", err, out)
	}
	if !strings.Contains(out, "50 200") {
		t.Errorf("stty size = %q, quería 50 200", out)
	}
}

// Colgar (cerrar la conexión) tiene que llevarse la shell por delante: una
// microVM no debe quedarse con sesiones de nadie.
func TestShellAlColgarMuereLaShell(t *testing.T) {
	rwc, _ := abrirShell(t, api.ShellRequest{Cmd: []string{"/bin/sh", "-c", "sleep 60"}})
	// El marcador es el propio proceso: si sigue vivo tras colgar, esto falla en
	// el cleanup del test al no poder cerrar el servidor.
	time.Sleep(300 * time.Millisecond)
	rwc.Close()
	time.Sleep(hangupGrace + time.Second)
}

// Sin la cabecera de upgrade no hay sesión: contesta un error normal, no un
// secuestro a medias.
func TestShellExigeUpgrade(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/exec/pty", ShellHandler(nil))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/exec/pty", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("status %d, want 426", resp.StatusCode)
	}
}

// Y un TERM inventado no acaba en el entorno de un proceso.
func TestShellValidaTerm(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/exec/pty", ShellHandler(nil))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, _ := json.Marshal(api.ShellRequest{Term: "xterm; rm -rf /"})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/exec/pty", bytes.NewReader(body))
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", api.ShellProto)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}
