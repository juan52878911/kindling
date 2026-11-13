package credproxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSplitHeaderIdaYVuelta(t *testing.T) {
	for _, port := range []int{80, 5432, 3306, 1, 65535} {
		h, err := SplitHeader(port)
		if err != nil {
			t.Fatal(err)
		}
		got, err := LeerSplitHeader(bytes.NewReader(h[:]))
		if err != nil || got != port {
			t.Fatalf("puerto %d: leído %d, %v", port, got, err)
		}
	}
	for _, port := range []int{0, -1, 65536} {
		if _, err := SplitHeader(port); !errors.Is(err, ErrSplitHeader) {
			t.Errorf("puerto %d: quería ErrSplitHeader, %v", port, err)
		}
	}
	for _, b := range [][]byte{
		{}, {'K', 1, 0}, // corta
		{'X', 1, 0, 80}, // otra marca
		{'K', 2, 0, 80}, // otra versión
		{'K', 1, 0, 0},  // puerto 0
	} {
		if _, err := LeerSplitHeader(bytes.NewReader(b)); !errors.Is(err, ErrSplitHeader) {
			t.Errorf("cabecera %v: quería ErrSplitHeader, %v", b, err)
		}
	}
}

// splitContra monta el servidor de reenvío de un proxy con una credencial
// HTTP para example.com cuyo proveedor es h, y devuelve dónde escucha.
func splitContra(t *testing.T, h http.HandlerFunc, permitir func(net.Conn) bool) (string, *int32) {
	t.Helper()
	var hits int32
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		h(w, r)
	}))
	t.Cleanup(up.Close)
	tr := up.Client().Transport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, up.Listener.Addr().String())
	}
	p := New(Options{Transport: tr})
	if _, err := p.SetCredentials([]Credential{{Env: "KEY", Domain: "example.com", Placeholder: testPlace, Secret: testSecret}}); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := NewSplitServer(p)
	s.Permitir = permitir
	hecho := make(chan error, 1)
	go func() { hecho <- s.Serve(l) }()
	t.Cleanup(func() {
		_ = s.Close()
		if err := <-hecho; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return l.Addr().String(), &hits
}

func conectarSplit(t *testing.T, addr string, port int) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	h, err := SplitHeader(port)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(h[:]); err != nil {
		t.Fatal(err)
	}
	return c
}

// Una conexión al 80 con su cabecera llega al proxy HTTP: el proveedor ve la
// clave y la vuelta, el marcador. Dos peticiones por la misma conexión
// (keep-alive), como haría un cliente del invitado.
func TestSplitServerHTTP(t *testing.T) {
	var recibida atomic.Value
	addr, hits := splitContra(t, func(w http.ResponseWriter, r *http.Request) {
		recibida.Store(r.Header.Get("Authorization"))
		io.WriteString(w, "eco "+r.Header.Get("Authorization"))
	}, nil)
	c := conectarSplit(t, addr, 80)
	br := bufio.NewReader(c)
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest("GET", "http://example.com/v1/x", nil)
		req.Header.Set("Authorization", "Bearer "+testPlace)
		if err := req.Write(c); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(br, req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || strings.Contains(string(body), testSecret) || !strings.Contains(string(body), testPlace) {
			t.Fatalf("petición %d: %d %q", i, resp.StatusCode, body)
		}
	}
	if got, _ := recibida.Load().(string); got != "Bearer "+testSecret {
		t.Fatalf("el proveedor recibió %q", got)
	}
	if atomic.LoadInt32(hits) != 2 {
		t.Fatalf("%d peticiones al proveedor, quería 2", atomic.LoadInt32(hits))
	}
}

// Sin credencial de base de datos, una conexión a otro puerto se cierra sin
// que nada le conteste; lo mismo una cabecera mala o un otro extremo que
// Permitir rechaza.
func TestSplitServerCierraLoQueNoEsSuyo(t *testing.T) {
	addr, hits := splitContra(t, func(http.ResponseWriter, *http.Request) {}, nil)
	cerrada := func(c net.Conn) bool {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := c.Read(make([]byte, 1))
		var ne net.Error
		return n == 0 && err != nil && !(errors.As(err, &ne) && ne.Timeout())
	}
	if c := conectarSplit(t, addr, 5432); !cerrada(c) {
		t.Error("una conexión al 5432 sin credencial de Postgres sigue abierta")
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	if !cerrada(c) {
		t.Error("una conexión sin cabecera sigue abierta")
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Error("algo llegó al proveedor")
	}

	addr2, hits2 := splitContra(t, func(http.ResponseWriter, *http.Request) {}, func(net.Conn) bool { return false })
	c2 := conectarSplit(t, addr2, 80)
	req, _ := http.NewRequest("GET", "http://example.com/", nil)
	req.Write(c2)
	if !cerrada(c2) || atomic.LoadInt32(hits2) != 0 {
		t.Error("Permitir false no cerró la conexión")
	}
}

// Close no se queda esperando a una conexión que no manda la cabecera ni a
// una petición HTTP abierta.
func TestSplitServerCloseCortaLoPendiente(t *testing.T) {
	p := New(Options{})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := NewSplitServer(p)
	hecho := make(chan error, 1)
	go func() { hecho <- s.Serve(l) }()
	c1, _ := net.Dial("tcp", l.Addr().String()) // sin cabecera
	defer c1.Close()
	c2, _ := net.Dial("tcp", l.Addr().String())
	defer c2.Close()
	h, _ := SplitHeader(80)
	c2.Write(h[:]) // y ninguna petición
	time.Sleep(50 * time.Millisecond)
	fin := make(chan struct{})
	go func() { s.Close(); close(fin) }()
	select {
	case <-fin:
	case <-time.After(3 * time.Second):
		t.Fatal("Close se quedó esperando")
	}
	if err := <-hecho; err != nil {
		t.Fatal(err)
	}
}
