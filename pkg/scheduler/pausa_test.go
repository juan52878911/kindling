package scheduler

import (
	"context"
	"testing"
	"time"
)

func gwPausas(budget int) (*Scheduler, map[string]bool, map[string]bool) {
	pausadas, congeladas := map[string]bool{}, map[string]bool{}
	g := &Scheduler{
		idle:      time.Minute,
		services:  map[string]*entry{},
		extra:     map[string][]*entry{},
		PausedMiB: budget,
		pop:       &popularity{ema: map[string]float64{}, pend: map[string]int{}},
		pauseFn:   func(id string) error { pausadas[id] = true; return nil },
		freezeFn:  func(id string) error { congeladas[id] = true; return nil },
	}
	return g, pausadas, congeladas
}

// Con presupuesto, se pausa lo que más puntúa por MiB: la tarea pequeña y
// popular antes que el modelo grande, y lo que no cabe se congela.
func TestDormirPausaLoPequenoYPopular(t *testing.T) {
	g, pausadas, congeladas := gwPausas(256)
	for i := 0; i < 10; i++ {
		g.pop.observe("chispa")
	}
	for i := 0; i < 20; i++ {
		g.pop.observe("von")
	}
	g.pop.observe("fria")
	g.dormir(context.Background(), []dormida{
		{"von", "m-von", 1536},  // más popular, pero no cabe
		{"chispa", "m-ch", 128}, // 10/128
		{"fria", "m-fria", 128}, // 1/128: puntúa menos, pero aún cabe
		{"nadie", "m-nadie", 64},
	})
	if !pausadas["m-ch"] || !pausadas["m-fria"] {
		t.Errorf("pausadas = %v; esperaba chispa y fria (caben 256)", pausadas)
	}
	if !congeladas["m-von"] || pausadas["m-von"] {
		t.Errorf("von debía congelarse: pausadas=%v congeladas=%v", pausadas, congeladas)
	}
	if !congeladas["m-nadie"] {
		t.Errorf("sin popularidad no se pausa: congeladas=%v", congeladas)
	}
	// El presupuesto se cuenta con lo ya pausado.
	g.pop.observe("otra")
	g.dormir(context.Background(), []dormida{{"otra", "m-otra", 64}})
	if pausadas["m-otra"] || !congeladas["m-otra"] {
		t.Errorf("sin presupuesto libre, otra debía congelarse")
	}
}

// Sin presupuesto, todo se congela, como siempre.
func TestDormirSinPresupuestoCongela(t *testing.T) {
	g, pausadas, congeladas := gwPausas(0)
	g.pop.observe("chispa")
	g.dormir(context.Background(), []dormida{{"chispa", "m-ch", 64}})
	if len(pausadas) != 0 || !congeladas["m-ch"] {
		t.Errorf("pausadas=%v congeladas=%v", pausadas, congeladas)
	}
}

// Una pausada que se enfría se congela de verdad, y evictLRU la sacrifica
// antes que a nada despierto.
func TestPausadasSeEnfrianYSonLasPrimerasEnCaer(t *testing.T) {
	g, _, congeladas := gwPausas(512)
	g.pausadas = map[string]pausada{
		"m-vieja": {service: "a", memMiB: 64, at: time.Now().Add(-time.Hour)},
		"m-nueva": {service: "b", memMiB: 64, at: time.Now()},
	}
	g.enfriarPausadas(context.Background())
	if !congeladas["m-vieja"] || congeladas["m-nueva"] {
		t.Fatalf("congeladas = %v; solo la vieja", congeladas)
	}
	g.services["despierta"] = &entry{machineID: "m-desp", lastUse: time.Now().Add(-time.Hour)}
	if got := g.evictLRU(context.Background(), "otro", ""); got != "b" {
		t.Errorf("evictLRU = %q; debía congelar la pausada b", got)
	}
	if congeladas["m-desp"] {
		t.Errorf("no debía tocar la despierta")
	}
}
