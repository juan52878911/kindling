package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// pausadaConFreezeParado monta una pausada vieja (m1, del servicio svc) en el
// daemon falso y en el registro, con un freeze que se detiene al entrar hasta
// que el test lo suelta: es la ventana entre que el planificador elige
// congelarla y que el daemon la congela de verdad.
func pausadaConFreezeParado(t *testing.T) (g *Scheduler, d *daemonFalso, dentro, soltar chan struct{}) {
	t.Helper()
	mc := instanciaVieja("m1")
	mc.State = api.StatePaused
	d = &daemonFalso{maquinas: map[string]*api.Machine{"m1": mc}}
	g = conDaemonFalso(t, d)
	g.pausadas = map[string]pausada{"m1": {service: "svc", memMiB: 64, at: time.Now().Add(-time.Hour)}}
	dentro, soltar = make(chan struct{}), make(chan struct{})
	g.freezeFn = func(id string) error {
		close(dentro)
		<-soltar
		_, err := g.client.Freeze(context.Background(), id)
		return err
	}
	return g, d, dentro, soltar
}

// El segador (enfriarPausadas) o evictLRU elegían una pausada para congelarla
// sin el candado de quien la despierta: un acquire concurrente la veía
// "paused" en su List(), la reanudaba, y el freeze, que llegaba después,
// congelaba la instancia recién adoptada (502 en su primera petición). Se
// prueba la intercalación por los dos caminos que congelan pausadas y por los
// dos acquire que pueden reanudarla: el de ensure (con el candado del
// servicio) y el del scale-out (fresh, sin él).
func TestCongelarPausadaNoPisaUnAcquireConcurrente(t *testing.T) {
	congeladores := map[string]func(g *Scheduler){
		"enfriar": func(g *Scheduler) { g.enfriarPausadas(context.Background()) },
		"evict":   func(g *Scheduler) { g.evictLRU(context.Background(), "otro", "") },
	}
	acquires := map[string]func(g *Scheduler) (*api.Machine, string, error){
		"ensure": func(g *Scheduler) (*api.Machine, string, error) {
			l := g.ensureLock("svc")
			l.Lock()
			defer l.Unlock()
			return g.acquire(context.Background(), "svc", false, nil)
		},
		"scaleout": func(g *Scheduler) (*api.Machine, string, error) {
			return g.acquire(context.Background(), "svc", true, nil)
		},
	}
	for nc, congelar := range congeladores {
		for na, adquirir := range acquires {
			t.Run(nc+"/"+na, func(t *testing.T) {
				g, d, dentro, soltar := pausadaConFreezeParado(t)
				fin := make(chan struct{})
				go func() { defer close(fin); congelar(g) }()
				<-dentro

				type res struct {
					mc  *api.Machine
					how string
					err error
				}
				hecho := make(chan res, 1)
				go func() {
					mc, how, err := adquirir(g)
					hecho <- res{mc, how, err}
				}()
				// Sin el arreglo el acquire acaba aquí mismo, reanudándola; con
				// él, el de ensure espera al candado y el del scale-out la salta.
				select {
				case r := <-hecho:
					hecho <- r // se reencola para leerlo abajo
				case <-time.After(300 * time.Millisecond):
				}
				close(soltar)
				<-fin
				r := <-hecho

				if r.err == nil && r.mc != nil && r.mc.ID == "m1" {
					if st := d.estado("m1"); st != api.StateRunning {
						t.Fatalf("acquire adoptó m1 (%s) y el freeze la dejó %q: la primera petición daría 502", r.how, st)
					}
				}
				if d.visto("POST /machines/m1/freeze") != 1 {
					t.Errorf("m1 debía congelarse una vez (%d)", d.visto("POST /machines/m1/freeze"))
				}
				g.mu.Lock()
				_, sigue := g.pausadas["m1"]
				marcada := g.adquiriendo["m1"]
				g.mu.Unlock()
				if sigue {
					t.Error("m1 se congeló pero sigue en el registro de pausadas")
				}
				if marcada && (r.mc == nil || r.mc.ID != "m1") {
					t.Error("m1 quedó marcada en adquiriendo sin que nadie la adquiriera")
				}
			})
		}
	}
}

// Con el candado de quien la despertaría ocupado, la pausada no se toca: se
// queda en el registro, sin congelar, para la siguiente vuelta. El de una
// aislada es el de su sesión, no el del servicio.
func TestCongelarPausadaRespetaElCandadoDeQuienLaDespierta(t *testing.T) {
	vieja := time.Now().Add(-time.Hour)
	casos := []struct {
		nombre  string
		p       pausada
		ocupado func(g *Scheduler) func()
		congela bool
	}{
		{"servicio ocupado", pausada{service: "svc", memMiB: 64, at: vieja},
			func(g *Scheduler) func() { l := g.ensureLock("svc"); l.Lock(); return l.Unlock }, false},
		{"sesion ocupada", pausada{service: "svc", memMiB: 64, at: vieja, aislada: "k"},
			func(g *Scheduler) func() { l := g.aisladaLock("k"); l.Lock(); return l.Unlock }, false},
		// La de una aislada no espera al candado del servicio: no la despierta
		// ensure, sino isolatedSession bajo el de su sesión.
		{"aislada con el servicio ocupado", pausada{service: "svc", memMiB: 64, at: vieja, aislada: "k"},
			func(g *Scheduler) func() { l := g.ensureLock("svc"); l.Lock(); return l.Unlock }, true},
	}
	for _, c := range casos {
		for _, camino := range []string{"enfriar", "evict"} {
			t.Run(c.nombre+"/"+camino, func(t *testing.T) {
				g, _, congeladas := gwPausas(512)
				g.pausadas = map[string]pausada{"m1": c.p}
				suelta := c.ocupado(g)
				if camino == "enfriar" {
					g.enfriarPausadas(context.Background())
				} else {
					g.evictLRU(context.Background(), "otro", "")
				}
				suelta()
				if congeladas["m1"] != c.congela {
					t.Fatalf("congelada = %v; quería %v", congeladas["m1"], c.congela)
				}
				_, sigue := g.pausadas["m1"]
				if sigue == c.congela {
					t.Errorf("en el registro = %v tras congelar = %v", sigue, c.congela)
				}
				if g.adquiriendo["m1"] {
					t.Error("m1 quedó marcada en adquiriendo")
				}
			})
		}
	}
}

// evictLRU no se rinde con la pausada más vieja si alguien la está
// despertando: sacrifica la siguiente.
func TestEvictLRUSaltaLaPausadaOcupada(t *testing.T) {
	g, _, congeladas := gwPausas(512)
	g.pausadas = map[string]pausada{
		"m-vieja": {service: "a", memMiB: 64, at: time.Now().Add(-2 * time.Hour)},
		"m-nueva": {service: "b", memMiB: 64, at: time.Now().Add(-time.Hour)},
	}
	l := g.ensureLock("a")
	l.Lock()
	got := g.evictLRU(context.Background(), "otro", "")
	l.Unlock()
	if got != "b" || !congeladas["m-nueva"] || congeladas["m-vieja"] {
		t.Fatalf("evictLRU = %q, congeladas %v; quería b sin tocar la de a", got, congeladas)
	}
	if _, sigue := g.pausadas["m-vieja"]; !sigue {
		t.Error("la pausada ocupada se perdió del registro")
	}
}
