package gateway

import (
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Solo es servicio MCP lo que se importó como tal y no es un modelo de IA: un
// golden de kling db o un punto de restauración no se listan (listarlos hacía
// que el catálogo los despertara, visto en el lab).
func TestEsServicioMCP(t *testing.T) {
	casos := []struct {
		s    api.Snapshot
		want bool
	}{
		{api.Snapshot{Name: "context7", Labels: map[string]string{"service": "context7"}}, true},
		{api.Snapshot{Name: "seqbundle", Labels: map[string]string{"service": "seqbundle", "stateful": "true"}}, true},
		{api.Snapshot{Name: "pg"}, false},
		{api.Snapshot{Name: "dbsnap-0ac0e6cf-p", Labels: map[string]string{"kling.db.golden": "pg", "kind": "sandbox"}}, false},
		{api.Snapshot{Name: "x86-smol", Labels: map[string]string{"service": "x86-smol", "von.model": "smol"}}, false},
		{api.Snapshot{Name: "x86-enc-e5", Labels: map[string]string{"service": "x86-enc-e5", "von.kind": "encoder"}}, false},
		{api.Snapshot{Name: "chispa-room", Labels: map[string]string{"service": "chispa-room", "chispa.task": "room"}}, false},
	}
	for _, c := range casos {
		if got := esServicioMCP(&c.s); got != c.want {
			t.Errorf("%s: %v, quería %v", c.s.Name, got, c.want)
		}
	}
}
