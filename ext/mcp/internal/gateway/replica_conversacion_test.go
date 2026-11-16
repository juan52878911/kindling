package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// UNA CONVERSACIÓN DEL AGREGADOR, UNA RÉPLICA.
//
// Con la primaria llena, el agregador escalaba a una réplica y guardaba allí la
// sesión, pero la llamada siguiente volvía a partir de la primaria: otra vez
// llena, otra réplica. Una réplica por tools/call hasta -max-replicas, y cada
// llamada caía en un proceso MCP nuevo, sin el estado de la anterior.

const cuerpoCallTool = `{"jsonrpc":"2.0","id":3,"method":"tools/call",` +
	`"params":{"name":"call_tool","arguments":{"name":"eco.echo","arguments":{}}}}`

// tresInvitados monta la primaria A (llena tras el initialize del catálogo) y
// dos máquinas más, B y C, que el daemon falso entrega por orden.
func tresInvitados(t *testing.T) (h http.Handler, a, b, c *invitadoRepetidor) {
	t.Helper()
	a = &invitadoRepetidor{nombre: "A", lleno: true}
	b = &invitadoRepetidor{nombre: "B"}
	c = &invitadoRepetidor{nombre: "C"}
	var addrs []string
	for _, v := range []*invitadoRepetidor{a, b, c} {
		srv := httptest.NewServer(v)
		t.Cleanup(srv.Close)
		addrs = append(addrs, strings.TrimPrefix(srv.URL, "http://"))
	}
	gw := New(api.NewClient(daemonDosMaquinas(t, "eco", addrs...)), 5*time.Minute, false, 0, "")
	return gw.Handler(""), a, b, c
}

func contar(v *invitadoRepetidor, prefijo string) int {
	n := 0
	for _, l := range v.recibidos() {
		if strings.HasPrefix(l, prefijo) {
			n++
		}
	}
	return n
}

func abrirAgregado(t testing.TB, h http.Handler) string {
	t.Helper()
	rec := pedir(t, h, "POST", "/mcp/_all", "", cuerpoInit)
	sid := rec.Header().Get(SessionHeader)
	if rec.Code != 200 || sid == "" {
		t.Fatalf("initialize de _all: %d %s", rec.Code, rec.Body)
	}
	return sid
}

func TestAgregadorNoCreaUnaReplicaPorLlamada(t *testing.T) {
	h, a, b, c := tresInvitados(t)
	sid := abrirAgregado(t, h)

	for i := range 3 {
		rec := pedir(t, h, "POST", "/mcp/_all", sid, cuerpoCallTool)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"soy":"B"`) {
			t.Fatalf("llamada %d: %d %s (quería la réplica B)", i+1, rec.Code, rec.Body)
		}
	}
	b.mu.Lock()
	inits := b.inits
	b.mu.Unlock()
	if inits != 1 {
		t.Fatalf("B recibió %d initialize, quería 1: la conversación perdió su sesión", inits)
	}
	if n := len(c.recibidos()); n != 0 {
		t.Fatalf("se creó una segunda réplica para la misma conversación: C recibió %v", c.recibidos())
	}
	// A solo vio el initialize del catálogo y el rechazado.
	if n := contar(a, "POST /mcp "); n != 3 {
		t.Fatalf("A recibió %d POST, quería 3 (catálogo, tools/list, initialize rechazado): %v", n, a.recibidos())
	}
}

// Varias llamadas a la vez de una conversación que aún no tiene sesión: la que
// escala deja la réplica en backing, y las que esperaban el candado la usan.
func TestAgregadorLlamadasConcurrentesUnaReplica(t *testing.T) {
	h, _, b, c := tresInvitados(t)
	sid := abrirAgregado(t, h)

	var wg sync.WaitGroup
	errs := make(chan string, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := pedir(t, h, "POST", "/mcp/_all", sid, cuerpoCallTool)
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"soy":"B"`) {
				errs <- fmt.Sprintf("%d %s", rec.Code, rec.Body)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("llamada concurrente: %s", e)
	}
	b.mu.Lock()
	inits := b.inits
	b.mu.Unlock()
	if inits != 1 || len(c.recibidos()) != 0 {
		t.Fatalf("B: %d initialize; C: %v (quería 1 y nada)", inits, c.recibidos())
	}
}

// Cerrar la conversación cierra su sesión en el puente de la réplica: si no,
// el proceso MCP queda huérfano hasta que el puente lo recoge por inactividad.
func TestAgregadorCerrarSueltaLasSesionesDelPuente(t *testing.T) {
	h, _, b, _ := tresInvitados(t)
	sid := abrirAgregado(t, h)
	if rec := pedir(t, h, "POST", "/mcp/_all", sid, cuerpoCallTool); rec.Code != 200 {
		t.Fatalf("tools/call: %d %s", rec.Code, rec.Body)
	}
	if rec := pedir(t, h, "DELETE", "/mcp/_all", sid, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", rec.Code, rec.Body)
	}
	// El cierre va en segundo plano: se espera con plazo.
	limite := time.Now().Add(3 * time.Second)
	for contar(b, "DELETE /mcp "+sidRepetido) == 0 {
		if time.Now().After(limite) {
			t.Fatalf("B no recibió el DELETE de la sesión: %v", b.recibidos())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
