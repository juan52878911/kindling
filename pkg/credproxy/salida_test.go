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
	"time"
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

// dnsFalso contesta por UDP en loopback a toda consulta A con ip, salvo las
// primeras `tira`, que se pierden como un datagrama en la red. Con nx, contesta
// NXDOMAIN. Cuenta las consultas recibidas.
func dnsFalso(t *testing.T, tira int32, ip [4]byte, nx bool) (string, *atomic.Int32) {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	var n atomic.Int32
	go func() {
		buf := make([]byte, 512)
		for {
			m, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n.Add(1) <= tira || m < 12 {
				continue
			}
			// Cabecera: mismo id, respuesta, RD+RA, una pregunta; luego la
			// pregunta tal cual y, si hay, un A que apunta al nombre (0xc00c).
			q := buf[:m]
			resp := append([]byte{}, q[:2]...)
			if nx {
				resp = append(resp, 0x81, 0x83, 0, 1, 0, 0, 0, 0, 0, 0)
			} else {
				resp = append(resp, 0x81, 0x80, 0, 1, 0, 1, 0, 0, 0, 0)
			}
			// Solo la pregunta: Go añade detrás un registro EDNS (OPT).
			fin := 12
			for fin < m && q[fin] != 0 {
				fin += int(q[fin]) + 1
			}
			fin += 5 // el 0 final, tipo y clase
			if fin > m {
				continue
			}
			resp = append(resp, q[12:fin]...)
			if !nx {
				resp = append(resp, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4)
				resp = append(resp, ip[:]...)
			}
			pc.WriteTo(resp, from)
		}
	}()
	return pc.LocalAddr().String(), &n
}

// Un datagrama DNS perdido no puede dejar al proxy sin IP: antes el plazo
// total (5 s) era el de un solo intento del resolver de Go y la petición
// salía con 502 a los 5000 ms.
func TestLookupPublicIPv4ReintentaSiSePierdeLaConsulta(t *testing.T) {
	defer func(p time.Duration) { dnsPlazoIntento = p }(dnsPlazoIntento)
	dnsPlazoIntento = 200 * time.Millisecond

	srv, n := dnsFalso(t, 2, [4]byte{93, 184, 216, 34}, false)
	got := LookupPublicIPv4(context.Background(), srv, "api.example.com")
	if len(got) != 1 || got[0] != "93.184.216.34" {
		t.Fatalf("con 2 consultas perdidas = %v, quiero [93.184.216.34]", got)
	}
	if c := n.Load(); c != 3 {
		t.Fatalf("consultas = %d, quiero 3 (dos perdidas y la buena, solo A)", c)
	}

	// Si se pierden todas, vacío (falla cerrado) sin pasar de los intentos.
	srv, n = dnsFalso(t, 100, [4]byte{93, 184, 216, 34}, false)
	if got := LookupPublicIPv4(context.Background(), srv, "api.example.com"); len(got) != 0 {
		t.Fatalf("sin respuesta = %v, quiero vacío", got)
	}
	if c := n.Load(); c != int32(dnsIntentos) {
		t.Fatalf("consultas sin respuesta = %d, quiero %d", c, dnsIntentos)
	}
}

// Un NXDOMAIN es definitivo: no se reintenta. Y una IP bloqueada no sale nunca.
func TestLookupPublicIPv4NoReintentaNXDOMAINNiDevuelveBloqueadas(t *testing.T) {
	srv, n := dnsFalso(t, 0, [4]byte{93, 184, 216, 34}, true)
	// Cuántas consultas hace una sola búsqueda de Go (según el sistema, prueba
	// también con los sufijos de búsqueda): LookupPublicIPv4 no debe hacer más.
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, srv)
	}}
	r.LookupNetIP(context.Background(), "ip4", "no.example.com")
	una := n.Load()
	if got := LookupPublicIPv4(context.Background(), srv, "no.example.com"); len(got) != 0 {
		t.Fatalf("NXDOMAIN = %v, quiero vacío", got)
	}
	if c := n.Load() - una; c != una {
		t.Fatalf("consultas con NXDOMAIN = %d, quiero %d (un solo intento)", c, una)
	}
	srv, _ = dnsFalso(t, 0, [4]byte{10, 0, 0, 5}, false)
	if got := LookupPublicIPv4(context.Background(), srv, "lan.example.com"); len(got) != 0 {
		t.Fatalf("IP privada = %v, quiero vacío", got)
	}
}
