package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
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
		{"von", "m-von", 1536, ""},  // más popular, pero no cabe
		{"chispa", "m-ch", 128, ""}, // 10/128
		{"fria", "m-fria", 128, ""}, // 1/128: puntúa menos, pero aún cabe
		{"nadie", "m-nadie", 64, ""},
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
	g.dormir(context.Background(), []dormida{{"otra", "m-otra", 64, ""}})
	if pausadas["m-otra"] || !congeladas["m-otra"] {
		t.Errorf("sin presupuesto libre, otra debía congelarse")
	}
}

// Sin presupuesto, todo se congela, como siempre.
func TestDormirSinPresupuestoCongela(t *testing.T) {
	g, pausadas, congeladas := gwPausas(0)
	g.pop.observe("chispa")
	g.dormir(context.Background(), []dormida{{"chispa", "m-ch", 64, ""}})
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

// G-04: si el freeze de la pausada más vieja falla, no debe perderse del
// registro — quedaría huérfana, reteniendo su RAM sin que ningún ensure o
// evictLRU vuelva a verla, hasta que el TTL del daemon (2×idle) la atrapara.
func TestEnfriarPausadasReponeSiFallaElFreeze(t *testing.T) {
	g, _, congeladas := gwPausas(512)
	g.freezeFn = func(id string) error { return errors.New("boom") }
	original := pausada{service: "a", memMiB: 64, at: time.Now().Add(-time.Hour)}
	g.pausadas = map[string]pausada{"m-vieja": original}

	g.enfriarPausadas(context.Background())

	if congeladas["m-vieja"] {
		t.Fatal("freezeFn devuelve error: no debía figurar como congelada")
	}
	g.mu.Lock()
	p, ok := g.pausadas["m-vieja"]
	g.mu.Unlock()
	if !ok {
		t.Fatal("la pausada cuyo freeze falló se perdió del registro; queda huérfana reteniendo RAM")
	}
	if p != original {
		t.Errorf("pausada repuesta = %+v; quería el valor original %+v", p, original)
	}
}

// Mismo caso pero por el camino de evictLRU (pausadaMasVieja): si tampoco ahí
// se puede congelar, tiene que reponerse antes de seguir con otra víctima.
func TestEvictLRUReponePausadaSiFallaElFreeze(t *testing.T) {
	g, _, _ := gwPausas(512)
	g.freezeFn = func(id string) error { return errors.New("boom") }
	original := pausada{service: "p", memMiB: 64, at: time.Now().Add(-time.Hour)}
	g.pausadas = map[string]pausada{"m-pausada": original}

	g.evictLRU(context.Background(), "otro", "")

	g.mu.Lock()
	p, ok := g.pausadas["m-pausada"]
	g.mu.Unlock()
	if !ok {
		t.Fatal("la pausada cuyo freeze falló se perdió: queda huérfana reteniendo RAM sin dueño")
	}
	if p != original {
		t.Errorf("pausada repuesta = %+v; quería %+v", p, original)
	}
}

// El segador avisa (OnSleep) de cada instancia que duerme, con su dirección,
// ANTES de congelarla: el gateway de IA cierra ahí las conexiones que tenga
// hacia ella, que no sobreviven a un despertar. Las que siguen trabajando no.
func TestReapAvisaOnSleep(t *testing.T) {
	g, _, congeladas := gwPausas(0)
	g.MachineTTL = -1
	g.Port = 8000
	var avisos []string
	g.OnSleep = func(svc, addr string) {
		if congeladas["m-vieja"] {
			t.Error("OnSleep llegó después de congelar")
		}
		avisos = append(avisos, svc+" "+addr)
	}
	viejo := time.Now().Add(-time.Hour)
	g.services["a"] = &entry{machineID: "m-vieja", ip: "10.0.0.2", lastUse: viejo}
	g.extra["a"] = []*entry{{machineID: "m-ocupada", ip: "10.0.0.3", lastUse: viejo, inflight: 1}}
	g.reapOnce(context.Background())
	if len(avisos) != 1 || avisos[0] != "a 10.0.0.2:8000" {
		t.Fatalf("avisos = %v; quería solo la ociosa, con su dirección", avisos)
	}
	if !congeladas["m-vieja"] || congeladas["m-ocupada"] {
		t.Fatalf("congeladas = %v", congeladas)
	}
}

// El presupuesto automático sale de la foto de memoria del daemon.
func TestPresupuestoDeHost(t *testing.T) {
	despierta := func(pss int64) []api.ProcStat {
		return []api.ProcStat{{State: api.StateRunning, PSSMiB: pss}, {State: api.StatePaused, PSSMiB: 9999}}
	}
	for _, c := range []struct {
		nombre string
		ps     api.ProcStats
		want   int
	}{
		{"sin medir: el de siempre", api.ProcStats{}, presupuestoPausadasDef},
		{"daemon sin TotalMiB: media de lo disponible", api.ProcStats{AvailableMiB: 4000}, 2000},
		// 24 GiB al 60 %: la mitad de la física menos lo despierto (11288)
		// frente a la mitad de lo disponible (7372): manda lo disponible.
		{"Mac holgado", api.ProcStats{TotalMiB: 24576, AvailableMiB: 14745, Machines: despierta(1000)}, 7372},
		// Mucho despierto: manda la mitad de la física menos eso.
		{"mucho despierto", api.ProcStats{TotalMiB: 16384, AvailableMiB: 12000, Machines: despierta(7000)}, 1192},
		{"nivel en el freno", api.ProcStats{TotalMiB: 16384, AvailableMiB: 16384 * 25 / 100}, 0},
		{"despierto de sobra", api.ProcStats{TotalMiB: 16384, AvailableMiB: 8000, Machines: despierta(9000)}, 0},
	} {
		if got := presupuestoDeHost(&c.ps); got != c.want {
			t.Errorf("%s: presupuesto = %d, quería %d", c.nombre, got, c.want)
		}
	}
}

// Con el presupuesto automático, si el nivel de memoria baja se congelan las
// pausadas que ya no caben, de la más vieja a la más nueva; en el freno, todas.
func TestPausadasSeCongelanAlBajarLaMemoria(t *testing.T) {
	g, pausadas, congeladas := gwPausas(-1)
	ps := &api.ProcStats{TotalMiB: 16384, AvailableMiB: 12000}
	viejo := procStatsDe
	procStatsDe = func(context.Context, *Scheduler) (*api.ProcStats, error) { return ps, nil }
	t.Cleanup(func() { procStatsDe = viejo })

	g.pop.observe("a")
	g.pop.observe("b")
	g.dormir(context.Background(), []dormida{{"a", "m-a", 1536, ""}})
	time.Sleep(time.Millisecond) // que b sea la más nueva
	g.dormir(context.Background(), []dormida{{"b", "m-b", 1536, ""}})
	if !pausadas["m-a"] || !pausadas["m-b"] {
		t.Fatalf("con memoria de sobra debían pausarse las dos: %v", pausadas)
	}
	// Baja el nivel al 27 %: caben 2250 MiB, una sola.
	ps.AvailableMiB = 4500
	g.enfriarPausadas(context.Background())
	if !congeladas["m-a"] || congeladas["m-b"] {
		t.Fatalf("congeladas = %v; solo la más vieja (a)", congeladas)
	}
	// En el freno, lo pausado se congela y nada nuevo se pausa.
	ps.AvailableMiB = 3000
	g.enfriarPausadas(context.Background())
	if !congeladas["m-b"] {
		t.Fatalf("en el freno debía congelarse b: %v", congeladas)
	}
	g.pop.observe("c")
	g.dormir(context.Background(), []dormida{{"c", "m-c", 64, ""}})
	if pausadas["m-c"] || !congeladas["m-c"] {
		t.Errorf("en el freno no se pausa: pausadas=%v", pausadas)
	}
}
