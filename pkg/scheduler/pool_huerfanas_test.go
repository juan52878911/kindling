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
	g.PoolOwner = "A"
	g.pool = newPool(g, 2)
	vieja := time.Now().Add(-time.Hour)
	etq := func(owner string) map[string]string {
		return map[string]string{"pool": "true", "owner": owner, api.LabelService: "svc", LabelPoolOwner: "A"}
	}
	d.mu.Lock()
	d.maquinas["huerfana"] = &api.Machine{ID: "huerfana", State: api.StateWarm, CreatedAt: vieja, Labels: etq("mcp")}
	d.maquinas["enfondo"] = &api.Machine{ID: "enfondo", State: api.StateWarm, CreatedAt: vieja, Labels: etq("mcp")}
	d.maquinas["entregada"] = &api.Machine{ID: "entregada", State: api.StateWarm, CreatedAt: vieja, Labels: etq("mcp")}
	d.maquinas["reciente"] = &api.Machine{ID: "reciente", State: api.StateWarm, CreatedAt: time.Now(), Labels: etq("mcp")}
	d.maquinas["ajena"] = &api.Machine{ID: "ajena", State: api.StateWarm, CreatedAt: vieja, Labels: etq("otro")}
	d.maquinas["normal"] = &api.Machine{ID: "normal", State: api.StateWarm, CreatedAt: vieja,
		Labels: map[string]string{"owner": "mcp"}}
	// Del fondo de otro gateway, o sin dueño (anterior a la etiqueta): no se tocan.
	d.maquinas["deotro"] = &api.Machine{ID: "deotro", State: api.StateWarm, CreatedAt: vieja,
		Labels: map[string]string{"pool": "true", "owner": "mcp", LabelPoolOwner: "B"}}
	d.maquinas["sindueno"] = &api.Machine{ID: "sindueno", State: api.StateWarm, CreatedAt: vieja,
		Labels: map[string]string{"pool": "true", "owner": "mcp"}}
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
	for _, id := range []string{"enfondo", "entregada", "reciente", "ajena", "normal", "deotro", "sindueno"} {
		if !d.existe(id) {
			t.Errorf("el barrido se llevó %s", id)
		}
	}
}

// R6-I2 (R5): dos planificadores sobre el mismo daemon no se borran el fondo, y
// sin PoolOwner no se barre nada.
func TestBarridoDosPlanificadoresNoSeBorran(t *testing.T) {
	a, d := conDaemonAislado(t)
	b := New(a.client, time.Minute, false, 0)
	a.PoolOwner, b.PoolOwner = "A", "B"
	a.pool, b.pool = newPool(a, 2), newPool(b, 2)
	vieja := time.Now().Add(-time.Hour)
	d.mu.Lock()
	for _, p := range []struct{ id, dueno string }{{"de-a", "A"}, {"de-b", "B"}} {
		d.maquinas[p.id] = &api.Machine{ID: p.id, State: api.StateWarm, CreatedAt: vieja,
			Labels: a.pool.etiquetasFondo("svc")}
		d.maquinas[p.id].Labels[LabelPoolOwner] = p.dueno
	}
	d.mu.Unlock()
	ctx := context.Background()
	a.pool.barrerHuerfanas(ctx)
	b.pool.barrerHuerfanas(ctx)
	// Cada uno borra solo la suya (ninguno la conoce en memoria).
	if d.existe("de-a") || d.existe("de-b") {
		t.Fatalf("cada planificador debía barrer la suya: de-a=%v de-b=%v", d.existe("de-a"), d.existe("de-b"))
	}
	d.mu.Lock()
	d.maquinas["de-b2"] = &api.Machine{ID: "de-b2", State: api.StateWarm, CreatedAt: vieja,
		Labels: map[string]string{"pool": "true", LabelPoolOwner: "B"}}
	d.mu.Unlock()
	a.pool.barrerHuerfanas(ctx)
	if !d.existe("de-b2") {
		t.Fatal("A borró una máquina del fondo de B")
	}
	b.PoolOwner = ""
	b.pool.barrerHuerfanas(ctx)
	if !d.existe("de-b2") {
		t.Fatal("sin PoolOwner se barrió")
	}
}

// Una entregada sigue viva pasado 2×idle+2 min mientras dure su llamada
// (HoldTTL), y se olvida al soltarla.
func TestEntregadaViveMientrasDuraLaLlamada(t *testing.T) {
	g, d := conDaemonAislado(t)
	g.PoolOwner = "A"
	g.pool = newPool(g, 2)
	d.mu.Lock()
	d.maquinas["m1"] = &api.Machine{ID: "m1", State: api.StateWarm, CreatedAt: time.Now().Add(-time.Hour),
		Labels: map[string]string{"pool": "true", LabelPoolOwner: "A"}}
	d.mu.Unlock()
	g.pool.ready["svc"] = []*warmVM{{id: "m1", born: time.Now()}}
	if vm := g.pool.take("svc"); vm == nil {
		t.Fatal("take = nil")
	}
	soltar := g.pool.sostener("m1")
	// La llamada dura más que 2×idle+2 min.
	g.pool.mu.Lock()
	g.pool.entregadas["m1"].desde = time.Now().Add(-time.Hour)
	g.pool.mu.Unlock()
	g.pool.barrerHuerfanas(context.Background())
	if !d.existe("m1") {
		t.Fatal("el barrido borró una entregada en uso")
	}
	soltar()
	soltar() // idempotente
	g.pool.mu.Lock()
	_, sigue := g.pool.entregadas["m1"]
	g.pool.mu.Unlock()
	if sigue {
		t.Fatal("la entregada no se olvidó al terminar la llamada")
	}
}
