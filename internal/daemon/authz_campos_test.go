package daemon

import (
	"reflect"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// S8: como TestCamposRunClasificados, para las demás peticiones que crean
// máquinas en nombre de un inquilino. Un campo nuevo sin clasificar falla
// aquí antes de entrar abierto.
func TestCamposClasificados(t *testing.T) {
	for _, c := range []struct {
		tipo   any
		clasif map[string]bool
		tabla  string
	}{
		{api.SandboxRequest{}, camposSandbox, "camposSandbox"},
		{api.GraphNode{}, camposNodo, "camposNodo"},
		{api.ForkRequest{}, camposFork, "camposFork"},
		{api.Graph{}, camposGrafo, "camposGrafo"},
		{api.GraphEdge{}, camposArista, "camposArista"},
	} {
		rt := reflect.TypeOf(c.tipo)
		vistos := map[string]bool{}
		for i := 0; i < rt.NumField(); i++ {
			n := nombreJSON(rt.Field(i))
			if n == "" {
				continue
			}
			vistos[n] = true
			if _, ok := c.clasif[n]; !ok {
				t.Errorf("%s.%s (%q) sin clasificar en %s: ¿de inquilino o de admin?", rt.Name(), rt.Field(i).Name, n, c.tabla)
			}
		}
		for n := range c.clasif {
			if !vistos[n] {
				t.Errorf("%s clasifica %q, que %s ya no tiene", c.tabla, n, rt.Name())
			}
		}
	}
}

// S19: el grafo y sus aristas también tienen tabla de campos, y lo que pone
// el daemon (fork_of, id...) no lo trae un inquilino. Un nombre de grafo
// ocupado por otro da el mismo 409 neutro que un snapshot ajeno, sin
// confirmar que existe ni cómo borrarlo.
func TestAuthzGrafoCamposYNombreAjeno(t *testing.T) {
	s := servidorAuthz(t, politicaDePrueba(t))
	h := conStubs(s)
	for _, c := range []struct{ cuerpo, campo string }{
		{`{"graph":{"name":"g","fork_of":"grafo-b","nodes":{"n":{"image":"min"}}}}`, "graph fork_of is admin-only"},
		{`{"graph":{"name":"g","id":"gb00000000000001","nodes":{"n":{"image":"min"}}}}`, "graph id is admin-only"},
	} {
		if rr := como(t, h, uidA, "POST", "/graphs", c.cuerpo); rr.Code != 403 || !strings.Contains(rr.Body.String(), c.campo) {
			t.Errorf("%s: %d %s", c.cuerpo, rr.Code, rr.Body)
		}
	}
	rr := como(t, h, uidA, "POST", "/graphs", `{"graph":{"name":"grafo-b","nodes":{"n":{"image":"min"}}}}`)
	if rr.Code != 409 || !strings.Contains(rr.Body.String(), "is taken") || strings.Contains(rr.Body.String(), "graph rm") {
		t.Fatalf("nombre de un grafo ajeno: %d %s", rr.Code, rr.Body)
	}
	// El propio pasa al manager, que dice lo de siempre; uno libre, también.
	for _, n := range []string{"grafo-a", "libre"} {
		cuerpo := `{"graph":{"name":"` + n + `","nodes":{"n":{"image":"min"}},"edges":[]}}`
		if rr := como(t, h, uidA, "POST", "/graphs", cuerpo); rr.Code != 299 {
			t.Errorf("%s: %d %s", n, rr.Code, rr.Body)
		}
	}
}

// Un nodo de grafo de inquilino no trae lo que pone el daemon: machine_id
// nombraría una máquina que no es suya.
func TestAuthzNodoSinCamposDelDaemon(t *testing.T) {
	s := servidorAuthz(t, politicaDePrueba(t))
	h := conStubs(s)
	rr := como(t, h, uidA, "POST", "/graphs", `{"graph":{"name":"g","nodes":{"n":{"image":"min","machine_id":"bbbb000000000001"}}}}`)
	if rr.Code != 403 || !strings.Contains(rr.Body.String(), "machine_id is admin-only") {
		t.Fatalf("nodo con machine_id: %d %s", rr.Code, rr.Body)
	}
	if rr := como(t, h, uidA, "POST", "/graphs", `{"graph":{"name":"g","nodes":{"n":{"image":"min","ports":[80]}}}}`); rr.Code != 299 {
		t.Fatalf("nodo normal: %d %s", rr.Code, rr.Body)
	}
}
