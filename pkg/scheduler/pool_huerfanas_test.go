package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Tras caer el gateway, las pool=true del anterior no están en p.ready: el
// barrido las borra, y respeta las del fondo, las entregadas, las recientes y
// las ajenas.
func TestBarridoBorraPoolHuerfanas(t *testing.T) {
	g, d := conDaemonAislado(t)
	ctx := context.Background()
	g.MachineLabels = map[string]string{"owner": "mcp"}
	g.pool = newPool(g, 2)
	vieja := time.Now().Add(-time.Hour)
	etq := func(owner string) map[string]string {
		return map[string]string{"pool": "true", "owner": owner, api.LabelService: "svc"}
	}
	d.mu.Lock()
	d.maquinas["huerfana"] = &api.Machine{ID: "huerfana", State: api.StateWarm, CreatedAt: vieja, Labels: etq("mcp")}
	d.maquinas["enfondo"] = &api.Machine{ID: "enfondo", State: api.StateWarm, CreatedAt: vieja, Labels: etq("mcp")}
	d.maquinas["entregada"] = &api.Machine{ID: "entregada", State: api.StateWarm, CreatedAt: vieja, Labels: etq("mcp")}
	d.maquinas["reciente"] = &api.Machine{ID: "reciente", State: api.StateWarm, CreatedAt: time.Now(), Labels: etq("mcp")}
	d.maquinas["ajena"] = &api.Machine{ID: "ajena", State: api.StateWarm, CreatedAt: vieja, Labels: etq("otro")}
	d.maquinas["normal"] = &api.Machine{ID: "normal", State: api.StateWarm, CreatedAt: vieja,
		Labels: map[string]string{"owner": "mcp"}}
	d.mu.Unlock()
	g.pool.ready["svc"] = []*warmVM{{id: "enfondo", born: time.Now()}, {id: "entregada", born: time.Now()}}
	if vm := g.pool.take("svc"); vm == nil || vm.id != "enfondo" {
		t.Fatalf("take = %v", vm)
	}
	// "enfondo" salió por take (entregada); "entregada" sigue en el fondo.
	g.pool.barrerHuerfanas(ctx)
	if d.existe("huerfana") {
		t.Error("la huérfana pool=true sigue")
	}
	for _, id := range []string{"enfondo", "entregada", "reciente", "ajena", "normal"} {
		if !d.existe(id) {
			t.Errorf("el barrido se llevó %s", id)
		}
	}
}
