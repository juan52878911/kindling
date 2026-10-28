package guest

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// unaConexion es un net.Listener que entrega c una vez y luego espera a que
// lo cierren.
type unaConexion struct {
	c    net.Conn
	once sync.Once
	fin  chan struct{}
}

func (l *unaConexion) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.c })
	if c != nil {
		return c, nil
	}
	<-l.fin
	return nil, net.ErrClosed
}
func (l *unaConexion) Close() error   { close(l.fin); return nil }
func (l *unaConexion) Addr() net.Addr { return vsockAddr{} }

// parVsock da los dos extremos de un socketpair: el del servidor envuelto en
// vsockConn (como lo deja ListenVsock) y el del cliente como net.Conn.
func parVsock(t *testing.T) (*vsockConn, net.Conn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetNonblock(fds[0], true); err != nil {
		t.Fatal(err)
	}
	srv := newVsockConn(os.NewFile(uintptr(fds[0]), "vsock"), vsockAddr{cid: 3, port: 8080})
	f := os.NewFile(uintptr(fds[1]), "cliente")
	cli, err := net.FileConn(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	return srv, cli
}

// Con el keep-alive (lo que hace el daemon), cada petición tras la primera
// llegaba con el contexto ya cancelado: net/http no reconocía el plazo con que
// aborta su lectura de fondo en un *fs.PathError. Es el exec que fallaba con
// "context canceled".
func TestVsockConnKeepAliveNoCancelaElContexto(t *testing.T) {
	srvConn, cli := parVsock(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /x", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			_, _ = io.WriteString(w, "cancelado")
		case <-time.After(20 * time.Millisecond):
			_, _ = io.WriteString(w, "ok")
		}
	})
	l := &unaConexion{c: srvConn, fin: make(chan struct{})}
	hs := &http.Server{Handler: mux}
	go func() { _ = hs.Serve(l) }()
	defer hs.Close()

	var usado bool
	tr := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		if usado {
			return nil, errors.New("el cliente abrió otra conexión: no reutilizó la primera")
		}
		usado = true
		return cli, nil
	}}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	for i := 0; i < 4; i++ {
		resp, err := hc.Post("http://agente/x", "application/json", nil)
		if err != nil {
			t.Fatalf("petición %d: %v", i, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != "ok" {
			t.Fatalf("petición %d por la misma conexión: %q", i, b)
		}
	}
}

func TestVsockConnErroresComoLosDeNet(t *testing.T) {
	srvConn, cli := parVsock(t)
	defer cli.Close()
	_ = srvConn.SetReadDeadline(time.Now().Add(-time.Second))
	_, err := srvConn.Read(make([]byte, 1))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("plazo vencido: %T %v, quiero un net.Error con Timeout()", err, err)
	}
	if _, ok := err.(net.Error); !ok {
		t.Fatalf("net/http mira err.(net.Error) sin errors.As: %T", err)
	}
	_ = srvConn.SetReadDeadline(time.Time{})

	if err := srvConn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("tras CloseWrite el otro extremo lee %v, quiero EOF", err)
	}
	srvConn.Close()
	if _, err := srvConn.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("leer cerrada: %v, quiero net.ErrClosed", err)
	}
}

func TestVsockRequested(t *testing.T) {
	for cmd, want := range map[string]bool{
		"console=hvc0 kling.vsock=1": true,
		"console=hvc0 kling.vsock=0": false,
		"console=hvc0":               false,
	} {
		p := parseBootParams(cmd)
		if got := vsockRequested(p); got != want {
			t.Errorf("%q: %v, quiero %v", cmd, got, want)
		}
		if len(p.unknown) != 0 {
			t.Errorf("%q: kling.vsock no debe avisar como desconocido: %v", cmd, p.unknown)
		}
	}
}
