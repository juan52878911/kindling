package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// ok es el handler protegido: si responde 200 es que la petición pasó.
var ok = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
})

func TestAuth(t *testing.T) {
	const token = "un-token-cualquiera"
	h := Auth(ok, token)

	cases := []struct {
		nombre string
		path   string
		header string
		want   int
	}{
		{"sin cabecera", "/mcp/_all", "", http.StatusUnauthorized},
		{"token equivocado", "/mcp/_all", "Bearer otro", http.StatusUnauthorized},
		{"prefijo correcto pero incompleto", "/mcp/_all", "Bearer un-token", http.StatusUnauthorized},
		{"sin esquema", "/mcp/_all", token, http.StatusUnauthorized},
		{"esquema equivocado", "/mcp/_all", "Basic " + token, http.StatusUnauthorized},
		{"token correcto", "/mcp/_all", "Bearer " + token, http.StatusOK},
		// El RFC 7235 declara el esquema insensible a mayúsculas y hay
		// clientes que lo mandan en minúscula.
		{"esquema en minúscula", "/mcp/_all", "bearer " + token, http.StatusOK},
		{"proxy de servicio protegido", "/mcp/files", "", http.StatusUnauthorized},
		{"inventario protegido", "/services", "", http.StatusUnauthorized},
		// /healthz abierto: systemd tiene que poder preguntarlo sin token.
		{"healthz sin token", "/healthz", "", http.StatusOK},
	}

	for _, c := range cases {
		t.Run(c.nombre, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, c.path, nil)
			if c.header != "" {
				r.Header.Set("Authorization", c.header)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Errorf("%s %s con %q: got %d, want %d", r.Method, c.path, c.header, w.Code, c.want)
			}
		})
	}
}

// Un token vacío desactiva la autenticación. Es lo que hace `-no-auth`, y solo
// se permite en loopback: ver TestIsLoopback y resolveGatewayToken.
func TestAuthTokenVacioNoEnvuelve(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/mcp/_all", nil)
	w := httptest.NewRecorder()
	Auth(ok, "").ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("con token vacío debería pasar todo: got %d", w.Code)
	}
}

// El 401 tiene que decir cómo autenticarse, o el cliente no sabe qué le falta.
func TestAuthAnunciaElEsquema(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/mcp/_all", nil)
	w := httptest.NewRecorder()
	Auth(ok, "t").ServeHTTP(w, r)
	if got := w.Header().Get("WWW-Authenticate"); got == "" {
		t.Error("un 401 sin WWW-Authenticate deja al cliente adivinando")
	}
}

// pprof tiene que quedar DETRÁS del token, no al lado de /healthz.
//
// Auth envuelve el mux entero, así que sale gratis — pero es justo el tipo de
// cosa que alguien "arregla" añadiéndole una exención cuando le estorba, y un
// volcado de goroutines o la línea de comandos completa no son cosas que deba
// poder pedir quien no tenga el token.
func TestPprofQuedaDetrasDelToken(t *testing.T) {
	g := &Gateway{PprofEnabled: true}
	h := g.Handler("s3cr3t")

	rutas := []string{
		"/debug/pprof/",
		"/debug/pprof/cmdline",
		"/debug/pprof/heap",
		"/debug/pprof/goroutine",
	}
	for _, ruta := range rutas {
		r := httptest.NewRequest(http.MethodGet, ruta, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s sin token devolvió %d, want 401", ruta, w.Code)
		}
	}

	// Y con el token sí responde: el guard no puede ser que esté roto.
	r := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	r.Header.Set("Authorization", "Bearer s3cr3t")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusUnauthorized {
		t.Error("con el token correcto pprof debería responder")
	}
}

// Sin -pprof no se registra nada: el 404 confirma que el flag apagado no deja
// las rutas puestas esperando a que alguien acierte el token.
func TestSinPprofNoHayRutas(t *testing.T) {
	g := &Gateway{PprofEnabled: false}
	r := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	r.Header.Set("Authorization", "Bearer s3cr3t")
	w := httptest.NewRecorder()
	g.Handler("s3cr3t").ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("con -pprof apagado debería ser 404, fue %d", w.Code)
	}
}
