package credproxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// proxyConAtacante monta un proxy con una credencial para example.com y un
// transporte que marca según el host de la URL saliente: example.com va al
// proveedor y cualquier otro a un servidor «atacante» cuyo certificado el
// transporte acepta (el atacante tiene uno válido para su dominio). Devuelve
// el proxy, la cabecera Authorization que vio el proveedor y cuántas veces se
// marcó o se pidió algo al atacante.
func proxyConAtacante(t *testing.T) (*httptest.Server, *atomic.Value, *int32) {
	t.Helper()
	recibida := &atomic.Value{}
	recibida.Store("")
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recibida.Store(r.Header.Get("Authorization"))
	}))
	t.Cleanup(up.Close)
	atacante := new(int32)
	mal := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(atacante, 1)
	}))
	t.Cleanup(mal.Close)
	tr := up.Client().Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.InsecureSkipVerify = true
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		if h, _, _ := net.SplitHostPort(addr); h == "example.com" {
			return d.DialContext(ctx, network, up.Listener.Addr().String())
		}
		atomic.AddInt32(atacante, 1)
		return d.DialContext(ctx, network, mal.Listener.Addr().String())
	}
	p := New(Options{Transport: tr})
	if _, err := p.SetCredentials([]Credential{{Env: "KEY", Domain: "example.com", Placeholder: testPlace, Secret: testSecret}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv, recibida, atacante
}

// crudo manda una petición HTTP/1.1 escrita a mano (un request-target que
// net/http no dejaría construir) y devuelve el estado de la respuesta.
func crudo(t *testing.T, srv *httptest.Server, linea string) int {
	t.Helper()
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, linea+"\r\nHost: example.com\r\nAuthorization: Bearer "+testPlace+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Un request-target opaco (http:@attacker.example/x) con Host: example.com
// elegía la credencial por el Host pero salía hacia
// https://example.com@attacker.example/x: la clave iba al atacante. Toda forma
// de request-target que no sea una ruta absoluta del mismo Host se rechaza sin
// salir.
func TestProxyRequestTargetRaroNoSacaLaClave(t *testing.T) {
	srv, recibida, atacante := proxyConAtacante(t)
	for _, linea := range []string{
		"GET http:@attacker.example/x HTTP/1.1",
		"GET https:@attacker.example/x HTTP/1.1",
		"GET http:attacker.example/x HTTP/1.1",
		"GET http://user@example.com/x HTTP/1.1",
		"GET http://example.com@attacker.example/x HTTP/1.1",
		"GET http://attacker.example/x HTTP/1.1",
	} {
		recibida.Store("")
		code := crudo(t, srv, linea)
		if code < 400 {
			t.Errorf("%q: status %d, quería un rechazo", linea, code)
		}
		if got := recibida.Load().(string); got != "" {
			t.Errorf("%q: el proveedor recibió %q", linea, got)
		}
	}
	if n := atomic.LoadInt32(atacante); n != 0 {
		t.Fatalf("el atacante recibió %d conexiones o peticiones", n)
	}
	// El absolute-form del propio Host sigue valiendo (un cliente que habla
	// al proxy como a un proxy HTTP) y sale con su ruta hacia ese Host.
	if code := crudo(t, srv, "GET http://example.com/v1/x?a=1 HTTP/1.1"); code != 200 {
		t.Fatalf("absolute-form del propio Host: status %d", code)
	}
	if got := recibida.Load().(string); got != "Bearer "+testSecret {
		t.Fatalf("el proveedor recibió %q", got)
	}
}

// Lo que net/http no deja llegar al handler (OPTIONS * lo contesta el propio
// servidor) se comprueba igual: otro servidor que sirva el Proxy podría
// pasarlo.
func TestDestinoRaro(t *testing.T) {
	for _, c := range []struct {
		u  url.URL
		ok bool
	}{
		{url.URL{Path: "/v1/x"}, true},
		{url.URL{Scheme: "http", Host: "example.com:80", Path: "/v1/x"}, true},
		{url.URL{Scheme: "http", Host: "EXAMPLE.com."}, true},
		{url.URL{Path: "*"}, false},
		{url.URL{Path: "@attacker.example/x"}, false},
		{url.URL{}, false},
		{url.URL{Scheme: "http", Opaque: "@attacker.example/x"}, false},
		{url.URL{Scheme: "ftp", Host: "example.com", Path: "/"}, false},
		{url.URL{Scheme: "http", Host: "example.com", User: url.User("u"), Path: "/"}, false},
		{url.URL{Scheme: "http", Host: "attacker.example", Path: "/"}, false},
	} {
		if got := destinoRaro(&c.u, "example.com"); (got == "") != c.ok {
			t.Errorf("%#v: motivo %q, quería ok=%v", c.u, got, c.ok)
		}
	}
}
