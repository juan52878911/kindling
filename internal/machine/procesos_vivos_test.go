package machine

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestAvisoProcesosVivos(t *testing.T) {
	procs := api.GuestProcs{Count: 3, Covered: 1, Names: []string{"node", "python3"}}
	m, id := managerConInvitado(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != api.GuestProcsPath {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(procs)
	}))
	mc := m.byID[id]
	mc.State = api.StateRunning

	// Un agente que no anuncia /procs no se pregunta.
	if a := m.avisoProcesosVivos(t.Context(), id); a != "" {
		t.Fatalf("sin la capacidad: %q", a)
	}
	mc.Agent = &api.GuestAgent{Caps: []string{api.GuestCapProcs}}
	a := m.avisoProcesosVivos(t.Context(), id)
	if !strings.HasPrefix(a, "2 processes are alive") || !strings.Contains(a, "(node, python3)") || !strings.Contains(a, "-on-restore") {
		t.Fatalf("aviso = %q", a)
	}
	// Todos del servicio con OnRestore: nada que decir.
	procs = api.GuestProcs{Count: 2, Covered: 2, Names: []string{"node"}}
	if a := m.avisoProcesosVivos(t.Context(), id); a != "" {
		t.Fatalf("todos cubiertos: %q", a)
	}
	// El puente MCP no avisa.
	procs = api.GuestProcs{Count: 1, Names: []string{"node"}}
	mc.Agent.Caps = append(mc.Agent.Caps, api.GuestCapMCP)
	if a := m.avisoProcesosVivos(t.Context(), id); a != "" {
		t.Fatalf("puente MCP: %q", a)
	}
}

func TestTextoProcesosVivosUno(t *testing.T) {
	if a := textoProcesosVivos(api.GuestProcs{Count: 1, Names: []string{"node"}}); !strings.HasPrefix(a, "1 process is alive in this snapshot (node)") {
		t.Fatalf("%q", a)
	}
}
