package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// envejecer deja la primaria de svc ociosa desde hace una hora.
func envejecer(t *testing.T, g *Scheduler, service string) *entry {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.services[service]
	if e == nil {
		t.Fatalf("%s no tiene primaria", service)
	}
	e.lastUse = time.Now().Add(-time.Hour)
	return e
}

// R8-01: una réplica descongelada carga su memoria de un clon del espejo del
// dorado (otro inodo) y deja de compartir la caché de páginas con las demás.
// Con IdleFor = remove el segador BORRA la ociosa en vez de congelarla, y la
// siguiente petición restaura del dorado.
func TestIdleRemoveBorraYRestaura(t *testing.T) {
	d := nuevoDaemonServicio()
	g := gwServicio(t, d, invitadoOK(t))
	g.IdleFor = func(string) IdlePolicy { return IdleRemove }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := g.ensure(ctx, "svc"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	envejecer(t, g, "svc")
	g.reapOnce(ctx)
	if n := d.contar("DELETE /machines/r1"); n != 1 {
		t.Errorf("DELETE de la ociosa = %d; quería 1", n)
	}
	if n := d.contar("POST /machines/r1/freeze"); n != 0 {
		t.Errorf("la ociosa se congeló (%d) con idle remove", n)
	}
	if n := d.contar("POST /machines/r1/pause"); n != 0 {
		t.Errorf("la ociosa se pausó (%d) con idle remove", n)
	}
	e, err := g.ensure(ctx, "svc")
	if err != nil {
		t.Fatalf("ensure tras el segador: %v", err)
	}
	if w := e.wake.Load(); w == nil || w.How != "restore" || e.machineID == "r1" {
		t.Errorf("la siguiente petición no restauró del dorado: %s (%+v)", e.machineID, w)
	}
}

// R8-01: con remove no existe el camino congelada → descongelada. Una
// congelada que quedó de antes (otra política, otro gateway) no se
// descongela: se restaura del dorado y la congelada se borra.
func TestIdleRemoveNoDescongela(t *testing.T) {
	d := nuevoDaemonServicio()
	d.poner(&api.Machine{ID: "w1", Name: "w1", State: api.StateWarm, IP: "127.0.0.1",
		Labels: map[string]string{api.LabelService: "svc"}, MemMiB: 128})
	g := gwServicio(t, d, invitadoOK(t))
	g.IdleFor = func(string) IdlePolicy { return IdleRemove }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e, err := g.ensure(ctx, "svc")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if e.machineID == "w1" || d.contar("POST /machines/w1/thaw") != 0 {
		t.Fatalf("se descongeló la congelada (instancia %s)", e.machineID)
	}
	// El borrado de las congeladas no retrasa la petición: va aparte.
	for limite := time.Now().Add(3 * time.Second); d.existe("w1"); {
		if time.Now().After(limite) {
			t.Fatal("la congelada no se borró")
		}
		time.Sleep(10 * time.Millisecond)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.adquiriendo["w1"] {
		t.Error("w1 quedó marcada en adquiriendo tras borrarla")
	}
}

// gwPausasPolitica es gwPausas con borrado de mentira y IdleFor.
func gwPausasPolitica(budget int, p IdlePolicy) (*Scheduler, map[string]bool, map[string]bool, map[string]bool) {
	g, pausadas, congeladas := gwPausas(budget)
	borradas := map[string]bool{}
	g.removeFn = func(_ context.Context, id string) error { borradas[id] = true; return nil }
	g.IdleFor = func(string) IdlePolicy { return p }
	return g, pausadas, congeladas, borradas
}

// IdlePause: pausa dentro del presupuesto y, si no cabe o vence PausedFor, la
// BORRA. Nunca congela: congelar y descongelar es justo lo que rompe el
// reparto de páginas (R8-01). El desalojo por memoria tampoco congela.
func TestIdlePauseNuncaCongela(t *testing.T) {
	g, pausadas, congeladas, borradas := gwPausasPolitica(64, IdlePause)
	g.pop.observe("svc")
	g.dormir(context.Background(), []dormida{
		{"svc", "m-cabe", 64, ""},
		{"svc", "m-nocabe", 64, ""},
	}, nil)
	if !pausadas["m-cabe"] {
		t.Errorf("la que cabe en el presupuesto no se pausó: pausadas=%v", pausadas)
	}
	if !borradas["m-nocabe"] {
		t.Errorf("la que no cabe no se borró: borradas=%v", borradas)
	}
	// Una pausada que se enfría se borra, no se congela.
	g.pausadas["m-cabe"] = pausada{service: "svc", memMiB: 64, at: time.Now().Add(-time.Hour)}
	g.enfriarPausadas(context.Background())
	if !borradas["m-cabe"] {
		t.Errorf("la pausada vieja no se borró: borradas=%v", borradas)
	}
	// El desalojo: lo pausado y lo despierto ocioso se borra.
	g.pausadas["m-p2"] = pausada{service: "svc", memMiB: 64, at: time.Now()}
	if got := g.evictLRU(context.Background(), "otro", ""); got != "svc" || !borradas["m-p2"] {
		t.Errorf("evictLRU = %q, borradas=%v; debía borrar la pausada", got, borradas)
	}
	g.services["svc"] = &entry{machineID: "m-desp", lastUse: time.Now().Add(-time.Hour)}
	if got := g.evictLRU(context.Background(), "otro", ""); got != "svc" || !borradas["m-desp"] {
		t.Errorf("evictLRU = %q, borradas=%v; debía borrar la despierta ociosa", got, borradas)
	}
	if len(congeladas) != 0 {
		t.Errorf("con idle pause se congeló %v", congeladas)
	}
}

// IdleRemove nunca pausa, aunque haya presupuesto y popularidad.
func TestIdleRemoveNuncaPausa(t *testing.T) {
	g, pausadas, congeladas, borradas := gwPausasPolitica(512, IdleRemove)
	g.pop.observe("svc")
	g.dormir(context.Background(), []dormida{{"svc", "m1", 64, ""}}, nil)
	if len(pausadas) != 0 || len(congeladas) != 0 || !borradas["m1"] {
		t.Errorf("pausadas=%v congeladas=%v borradas=%v; solo debía borrarse", pausadas, congeladas, borradas)
	}
}

// IdleFreeze es lo de hoy: pausa lo que cabe y congela el resto.
func TestIdleFreezeComoHoy(t *testing.T) {
	g, pausadas, congeladas, borradas := gwPausasPolitica(64, IdleFreeze)
	g.pop.observe("svc")
	g.dormir(context.Background(), []dormida{{"svc", "m1", 64, ""}, {"svc", "m2", 64, ""}}, nil)
	if !pausadas["m1"] || !congeladas["m2"] || len(borradas) != 0 {
		t.Errorf("pausadas=%v congeladas=%v borradas=%v", pausadas, congeladas, borradas)
	}
}
