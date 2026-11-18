package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// UNA CONVERSACIÓN FIJADA A UNA MÁQUINA QUE MURIÓ POR DEBAJO.
//
// El agregador vuelve a la instancia donde la conversación tiene sesión sin
// pasar por Ensure. Así se saltaba su comprobación de vida: si la máquina
// moría (un kling rm, el recolector del daemon), cada tools/call de esa
// conversación fallaba con «unreadable response» hasta que el latido del TTL
// la sacaba, o para siempre sin renew.

// conversacionEnA monta un servicio compartido con dos máquinas posibles (A y
// B, en ese orden), abre una conversación del agregador y la deja con sesión
// en A. Devuelve también la función que mata A por debajo: el daemon la olvida
// y su invitado deja de escuchar.
func conversacionEnA(t *testing.T) (gw *Gateway, h func(sid string) *httptest.ResponseRecorder, sid string, matarA func()) {
	t.Helper()
	a := &invitadoRepetidor{nombre: "A"}
	b := &invitadoRepetidor{nombre: "B"}
	sa := httptest.NewServer(a)
	sb := httptest.NewServer(b)
	t.Cleanup(sa.Close)
	t.Cleanup(sb.Close)
	d := &daemonAislado{service: "eco", compartido: true, invitados: []string{
		strings.TrimPrefix(sa.URL, "http://"), strings.TrimPrefix(sb.URL, "http://")}}
	gw = New(api.NewClient(levantarDaemonAislado(t, d)), 5*time.Minute, false, 0, "")
	hh := gw.Handler("")
	sid = abrirAgregado(t, hh)
	h = func(sid string) *httptest.ResponseRecorder {
		return pedir(t, hh, "POST", "/mcp/_all", sid, cuerpoCallTool)
	}
	for i := range 2 { // la segunda ya vuelve a A por la sesión, y confirma su vida
		if rec := h(sid); !strings.Contains(rec.Body.String(), `"soy":"A"`) {
			t.Fatalf("llamada %d antes de morir A: %d %s", i+1, rec.Code, rec.Body)
		}
	}
	matarA = func() {
		d.mu.Lock()
		delete(d.maquinas, "m1")
		d.mu.Unlock()
		sa.CloseClientConnections()
		sa.Close()
	}
	return gw, h, sid, matarA
}

// Pasado el tiempo de confianza, la primera llamada tras morir A ya va a otra
// máquina.
func TestConversacionFijadaAMaquinaMuertaCambiaDeMaquina(t *testing.T) {
	gw, h, sid, matarA := conversacionEnA(t)
	matarA()
	gw.agg.vidaMu.Lock()
	gw.agg.vidaTTL = 0 // como si hubieran pasado los 15 s
	gw.agg.vidaMu.Unlock()
	for i := range 3 {
		if rec := h(sid); !strings.Contains(rec.Body.String(), `"soy":"B"`) {
			t.Fatalf("llamada %d tras morir A: %d %s (la conversación sigue fijada a la máquina muerta)",
				i+1, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
}

// Dentro del tiempo de confianza, la llamada que choca con la máquina muerta
// falla con el error de verdad (no «unreadable response»), y la siguiente ya no
// se fía de la confirmación anterior: va a otra máquina.
func TestConversacionFijadaFallaUnaVezYSeRecupera(t *testing.T) {
	_, h, sid, matarA := conversacionEnA(t)
	matarA()
	rec := h(sid)
	if strings.Contains(rec.Body.String(), "unreadable response") {
		t.Fatalf("el error de transporte se escondió: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"soy":"B"`) && !strings.Contains(rec.Body.String(), "eco:") {
		t.Fatalf("primera llamada tras morir A: %d %s", rec.Code, rec.Body)
	}
	for i := range 2 {
		if rec := h(sid); !strings.Contains(rec.Body.String(), `"soy":"B"`) {
			t.Fatalf("llamada %d tras el fallo: %d %s", i+2, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
}

// invitadoCaducado es un puente que, con caducado, ya no conoce ninguna sesión
// (404 a tools/call) y está lleno (rechaza initialize por tope).
type invitadoCaducado struct {
	invitadoRepetidor
	caducado atomic.Bool
}

func (v *invitadoCaducado) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if v.caducado.Load() && r.Method == http.MethodPost {
		body, _ := api.LeerCuerpo(r.Body, 1<<20)
		if strings.Contains(string(body), `"initialize"`) {
			http.Error(w, "session limit reached (1)", http.StatusBadRequest)
			return
		}
		http.Error(w, "session unknown or expired", http.StatusNotFound)
		return
	}
	v.invitadoRepetidor.ServeHTTP(w, r)
}

// Si al rehacer el handshake de una sesión caducada la instancia está llena, la
// sesión caducada no se queda guardada: la llamada siguiente abre una nueva por
// el camino normal, que escala a una réplica. Antes cada llamada volvía a la
// sesión caducada y fallaba igual.
func TestReinitLlenoNoDejaLaSesionCaducada(t *testing.T) {
	a := &invitadoCaducado{invitadoRepetidor: invitadoRepetidor{nombre: "A"}}
	b := &invitadoRepetidor{nombre: "B"}
	sa := httptest.NewServer(a)
	sb := httptest.NewServer(b)
	t.Cleanup(sa.Close)
	t.Cleanup(sb.Close)
	d := &daemonAislado{service: "eco", compartido: true, invitados: []string{
		strings.TrimPrefix(sa.URL, "http://"), strings.TrimPrefix(sb.URL, "http://")}}
	hh := New(api.NewClient(levantarDaemonAislado(t, d)), 5*time.Minute, false, 0, "").Handler("")
	sid := abrirAgregado(t, hh)
	if rec := pedir(t, hh, "POST", "/mcp/_all", sid, cuerpoCallTool); !strings.Contains(rec.Body.String(), `"soy":"A"`) {
		t.Fatalf("antes de caducar: %d %s", rec.Code, rec.Body)
	}
	a.caducado.Store(true)
	_ = pedir(t, hh, "POST", "/mcp/_all", sid, cuerpoCallTool) // esta puede fallar: A está llena
	if rec := pedir(t, hh, "POST", "/mcp/_all", sid, cuerpoCallTool); !strings.Contains(rec.Body.String(), `"soy":"B"`) {
		t.Fatalf("tras caducar la sesión con A llena: %d %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}
