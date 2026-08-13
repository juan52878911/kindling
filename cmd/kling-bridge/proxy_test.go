package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"
)

// parseServiceSpec solo reconoce transport:http con puerto: cualquier otra cosa
// es modo stdio y devuelve nil, para que el caso mayoritario no pague nada.
func TestParseServiceSpec(t *testing.T) {
	casos := []struct {
		nombre string
		json   string
		quiere *serviceSpec
	}{
		{"http válido", `{"transport":"http","port":8090}`, &serviceSpec{Transport: "http", Port: 8090}},
		{"stdio explícito", `{"transport":"stdio"}`, nil},
		{"sin transporte", `{"port":8090}`, nil},
		{"http sin puerto", `{"transport":"http"}`, nil},
		{"http puerto cero", `{"transport":"http","port":0}`, nil},
		{"basura", `no soy json`, nil},
	}
	for _, c := range casos {
		got := parseServiceSpec([]byte(c.json))
		switch {
		case c.quiere == nil && got != nil:
			t.Errorf("%s: esperaba nil, salió %+v", c.nombre, got)
		case c.quiere != nil && got == nil:
			t.Errorf("%s: esperaba %+v, salió nil", c.nombre, c.quiere)
		case c.quiere != nil && (got.Transport != c.quiere.Transport || got.Port != c.quiere.Port):
			t.Errorf("%s: esperaba %+v, salió %+v", c.nombre, c.quiere, got)
		}
	}
}

// waitForPort vuelve en cuanto algo escucha, y falla si nadie abre a tiempo.
func TestWaitForPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := waitForPort(ln.Addr().String(), 2*time.Second); err != nil {
		t.Errorf("con un puerto abierto no debería fallar: %v", err)
	}

	// Un puerto que nadie abre: debe vencer el plazo con error.
	libre, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := libre.Addr().String()
	libre.Close() // ahora nadie escucha ahí
	if err := waitForPort(addr, 300*time.Millisecond); err == nil {
		t.Error("sin nadie escuchando debería fallar por plazo")
	}
}

// bridgeHaciaServidor arma un bridge en modo proxy apuntando a srv, sin lanzar
// ningún proceso: se prueba el ruteo/rastreo, no el ciclo de vida del hijo.
func bridgeHaciaServidor(t *testing.T, srv *httptest.Server) *bridge {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	b := &bridge{
		idle:     time.Minute,
		service:  &serviceSpec{Transport: "http", Port: port},
		sessions: map[string]*session{},
	}
	b.proxy = b.newReverseProxy(u)
	return b
}

// En modo proxy, el Mcp-Session-Id se rastrea desde AMBOS lados: el que el
// cliente manda en la petición, y el que el servidor asigna en la respuesta de
// un initialize (cuando el cliente aún no lo tenía).
func TestProxyRastreaSesion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Si la petición no traía sesión, el servidor la asigna (como en un
		// initialize real).
		if r.Header.Get(SessionHeader) == "" {
			w.Header().Set(SessionHeader, "asignada-por-servidor")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	b := bridgeHaciaServidor(t, srv)

	// (1) El servidor asigna la sesión en la respuesta: debe quedar rastreada.
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	b.handleProxy(httptest.NewRecorder(), req)
	b.mu.Lock()
	_, ok := b.proxySeen["asignada-por-servidor"]
	b.mu.Unlock()
	if !ok {
		t.Error("no se rastreó el Mcp-Session-Id que asignó el servidor")
	}

	// (2) El cliente manda su sesión: se anota su actividad.
	req = httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set(SessionHeader, "del-cliente")
	b.handleProxy(httptest.NewRecorder(), req)
	b.mu.Lock()
	_, ok = b.proxySeen["del-cliente"]
	b.mu.Unlock()
	if !ok {
		t.Error("no se rastreó el Mcp-Session-Id que mandó el cliente")
	}

	// (3) Un DELETE del cliente cierra la sesión: el puente la olvida.
	req = httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set(SessionHeader, "del-cliente")
	b.handleProxy(httptest.NewRecorder(), req)
	b.mu.Lock()
	_, ok = b.proxySeen["del-cliente"]
	b.mu.Unlock()
	if ok {
		t.Error("tras un DELETE la sesión debería estar olvidada")
	}
}

// El reaper de ociosas del modo proxy no mata el proceso compartido, pero SÍ
// manda un DELETE al servidor por cada sesión que lleva demasiado sin actividad,
// y la olvida.
func TestReapIdleProxyEnviaDelete(t *testing.T) {
	var mu sync.Mutex
	borradas := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			borradas[r.Header.Get(SessionHeader)] = true
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	b := bridgeHaciaServidor(t, srv)

	// Una sesión vieja (rebasa el idle) y otra reciente (no).
	b.proxySeen = map[string]time.Time{
		"vieja":    time.Now().Add(-2 * time.Hour),
		"reciente": time.Now(),
	}
	b.reapIdleProxy()

	mu.Lock()
	got := borradas["vieja"]
	noBorrada := borradas["reciente"]
	mu.Unlock()
	if !got {
		t.Error("la sesión vieja debería haber recibido un DELETE")
	}
	if noBorrada {
		t.Error("la sesión reciente no debía tocarse")
	}

	b.mu.Lock()
	_, sigueVieja := b.proxySeen["vieja"]
	_, sigueReciente := b.proxySeen["reciente"]
	b.mu.Unlock()
	if sigueVieja {
		t.Error("la sesión vieja debería estar olvidada")
	}
	if !sigueReciente {
		t.Error("la sesión reciente debería seguir rastreada")
	}
}
