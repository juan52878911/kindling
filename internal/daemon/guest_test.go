package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

type vistoPorElInvitado struct {
	path, method, accept, session, body string
}

func invitado(t *testing.T, respuesta string) (string, *vistoPorElInvitado) {
	t.Helper()
	v := &vistoPorElInvitado{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*v = vistoPorElInvitado{r.URL.Path, r.Method, r.Header.Get("Accept"), r.Header.Get("Mcp-Session-Id"), string(b)}
		w.Header().Set("Mcp-Session-Id", "sesion-1")
		w.Header().Set("X-Otra", "x")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, respuesta)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), v
}

// Con ruta explícita el daemon no añade nada de MCP: ni Accept ni la cabecera
// de sesión de vuelta, salvo que se pida.
func TestProxyGuestGenerico(t *testing.T) {
	addr, v := invitado(t, `hola`)
	out, _, err := proxyGuest(context.Background(), addr, api.GuestRequest{Path: "/healthz", Method: "GET"}, porDial(addr))
	if err != nil {
		t.Fatal(err)
	}
	if v.path != "/healthz" || v.method != "GET" || v.accept != "" {
		t.Fatalf("el proxy genérico metió algo de MCP: %+v", v)
	}
	if _, ok := out.Headers["Mcp-Session-Id"]; ok || out.Headers["Content-Type"] != "application/json" {
		t.Fatalf("sin response_headers solo vuelve Content-Type: %v", out.Headers)
	}

	out, _, err = proxyGuest(context.Background(), addr, api.GuestRequest{
		Path: "/mcp", Headers: map[string]string{"Mcp-Session-Id": "s0"},
		ResponseHeaders: []string{"X-Otra", "Mcp-Session-Id"},
	}, porDial(addr))
	if err != nil {
		t.Fatal(err)
	}
	if v.session != "s0" || out.Headers["X-Otra"] != "x" || out.Headers["Mcp-Session-Id"] != "sesion-1" {
		t.Fatalf("response_headers o cabeceras de ida: visto=%+v out=%v", v, out.Headers)
	}
}

func TestProxyGuestLimites(t *testing.T) {
	addr, _ := invitado(t, strings.Repeat("x", 2048))
	if _, code, err := proxyGuest(context.Background(), addr, api.GuestRequest{Path: "/", MaxBodyBytes: 1024}, porDial(addr)); err == nil || code != http.StatusBadGateway {
		t.Fatalf("una respuesta mayor que max_body_bytes debe fallar, no truncarse: code=%d err=%v", code, err)
	}
	if _, code, _ := proxyGuest(context.Background(), addr, api.GuestRequest{Path: "/", MaxBodyBytes: api.GuestMaxBodyCap + 1}, porDial(addr)); code != http.StatusBadRequest {
		t.Fatalf("pasarse del tope es un 400: %d", code)
	}
	if _, code, _ := proxyGuest(context.Background(), addr, api.GuestRequest{Path: "sin-barra"}, porDial(addr)); code != http.StatusBadRequest {
		t.Fatalf("una ruta sin '/' es un 400: %d", code)
	}
}

// Sin ruta ya no hay valores por defecto: quien habla con el invitado dice a
// dónde. Solo una sonda de puerto puede ir sin ruta.
func TestProxyGuestExigeRuta(t *testing.T) {
	addr, _ := invitado(t, `x`)
	if _, code, err := proxyGuest(context.Background(), addr, api.GuestRequest{Body: "{}"}, porDial(addr)); err == nil || code != 400 {
		t.Fatalf("sin ruta debe ser un 400: code=%d err=%v", code, err)
	}
	if _, _, err := proxyGuest(context.Background(), addr, api.GuestRequest{ProbeOnly: true, WaitMS: 1000}, porDial(addr)); err != nil {
		t.Fatalf("una sonda de puerto no necesita ruta: %v", err)
	}
}

// porDial es la espera de Linux: el puerto está abierto si se puede conectar.
func porDial(addr string) func(context.Context, time.Duration) error {
	return func(ctx context.Context, t time.Duration) error { return waitPort(ctx, addr, t) }
}

// probe_only tiene que esperar a que el puerto abra DENTRO del invitado, no a
// que acepte la dirección: en macOS el reenvío acepta siempre. Con una espera
// que nunca ve el puerto abierto, contesta 504 aunque addr acepte conexiones.
func TestProxyGuestProbeUsaLaEspera(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	nunca := func(ctx context.Context, t time.Duration) error { return errors.New("nobody is listening") }
	_, code, err := proxyGuest(context.Background(), ln.Addr().String(),
		api.GuestRequest{ProbeOnly: true, WaitMS: 100}, nunca)
	if err == nil || code != http.StatusGatewayTimeout {
		t.Fatalf("probe_only con el puerto cerrado dentro: code=%d err=%v; quería 504", code, err)
	}
}
