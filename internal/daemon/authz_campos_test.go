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
