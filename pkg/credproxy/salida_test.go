package credproxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// SetCredentials normaliza los dominios, devuelve los distintos ordenados (los
// que el resolver debe desviar) y rechaza un juego inválido sin tocar el que
// había.
func TestSetCredentialsDevuelveLosDominios(t *testing.T) {
	p := New(Options{})
	got, err := p.SetCredentials([]Credential{
		{Domain: "B.example.com.", Placeholder: testPlace, Secret: "x"},
		{Domain: "a.example.com", Placeholder: testPlace2, Secret: "y"},
		{Domain: "b.example.com", Placeholder: testPlace + "2", Secret: "z"},
	})
	if err != nil || strings.Join(got, ",") != "a.example.com,b.example.com" {
		t.Fatalf("dominios %v (%v)", got, err)
	}
	if _, err := p.SetCredentials([]Credential{{Domain: "*.example.com", Placeholder: testPlace, Secret: "x"}}); err == nil {
		t.Fatal("un comodín debería rechazarse")
	}
	if len(p.creds["b.example.com"]) != 2 {
		t.Errorf("un juego inválido no debe sustituir al anterior: %v", p.creds)
	}
}

// Aunque el resolver inyectado devuelva una IP privada (DNS envenenado), el
// transporte seguro no conecta: 502 y la clave no sale hacia la LAN.
func TestSalidaSeguraNoConectaAUnaIPBloqueada(t *testing.T) {
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { atomic.AddInt32(&hits, 1) }))
	defer up.Close()
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	p := New(Options{Lookup: func(context.Context, string) []string { return []string{"127.0.0.1", "10.1.2.3", "::1"} }})
	if _, err := p.SetCredentials([]Credential{{Domain: "example.com", Placeholder: testPlace, Secret: testSecret}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "example.com:" + port
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "public IPv4") {
		t.Errorf("status %d cuerpo %q; quería 502 por IP no pública", resp.StatusCode, body)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Errorf("se conectó a una IP bloqueada (%d peticiones)", hits)
	}
}

func TestIsBlockedIP(t *testing.T) {
	for _, s := range []string{"10.0.0.1", "172.30.0.1", "192.168.1.1", "169.254.169.254", "127.0.0.1", "100.64.0.1"} {
		if !IsBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s debería estar bloqueada", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "172.32.0.1"} {
		if IsBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s no debería estar bloqueada", s)
		}
	}
}
