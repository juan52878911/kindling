package gateway

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
)

// El token del gateway NO debe llegar al otro lado.
//
// `Authorization` no es hop-by-hop, asi que ReverseProxy la propaga verbatim. Y
// al otro lado hay, o bien un servidor MCP invitado —que este codigo declara
// HOSTIL por diseno—, o bien una URL de terceros puesta con `kling mcp link`.
// Con ese token, un invitado comprometido llama al gateway como cliente
// legitimo: despertar un snapshot ES ejecutar codigo.
func TestElTokenDelGatewayNoLlegaAlOtroLado(t *testing.T) {
	var visto string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		visto = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	u, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	// Los dos caminos que proxean con la credencial del cliente encima: la
	// microVM invitada y el enlace a una URL de terceros. Se construyen igual
	// que en produccion.
	enlace := httputil.NewSingleHostReverseProxy(u)
	sinCredencialDelGateway(enlace)
	proxies := map[string]http.Handler{
		"microVM invitada": proxyInvitado(u),
		"enlace externo":   enlace,
	}

	for nombre, h := range proxies {
		visto = ""
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer TOKEN-SECRETO-DEL-GATEWAY")
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req)

		if visto != "" {
			t.Errorf("%s: el backend recibio Authorization=%q; el token del gateway se ha filtrado", nombre, visto)
		}
	}
}

// Y lo contrario: quitar la credencial no debe romper el resto de la peticion.
func TestQuitarLaCredencialNoRompeElResto(t *testing.T) {
	var ct, cuerpo, ruta string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct = r.Header.Get("Content-Type")
		ruta = r.URL.Path
		b := make([]byte, 64)
		n, _ := r.Body.Read(b)
		cuerpo = string(b[:n])
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0"}`))
	req.Header.Set("Authorization", "Bearer X")
	req.Header.Set("Content-Type", "application/json")
	proxyInvitado(u).ServeHTTP(httptest.NewRecorder(), req)

	if ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if ruta != "/mcp" {
		t.Errorf("ruta = %q", ruta)
	}
	if !strings.Contains(cuerpo, "jsonrpc") {
		t.Errorf("el cuerpo no llego: %q", cuerpo)
	}
}
