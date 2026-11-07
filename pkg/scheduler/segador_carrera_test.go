package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// El segador sacaba del mapa la instancia ociosa, soltaba g.mu y después la
// congelaba. En esa ventana un acquire concurrente no la encontraba en el
// mapa, la veía "running" sin Transition en su List() (o en su Get), la
// adoptaba y le ataba sesiones; el freeze llegaba por debajo y la primera
// petición daba 502. Es la carrera que tomarPausada ya cerró para las
// pausadas, por los tres caminos que pueden adoptar una despierta: ensure
// (con el candado del servicio) sobre la primaria, un acquire sin ese
// candado sobre una réplica, y isolatedSession (con el de la sesión) sobre
// la máquina de una aislada.
func TestElSegadorNoCongelaDebajoDeUnAcquire(t *testing.T) {
	casos := map[string]struct {
		montar   func(g *Scheduler, e *entry)
		adquirir func(g *Scheduler) (*api.Machine, string, error)
	}{
		"primaria/ensure": {
			montar: func(g *Scheduler, e *entry) { g.services["svc"] = e },
			adquirir: func(g *Scheduler) (*api.Machine, string, error) {
				l := g.ensureLock("svc")
				l.Lock()
				defer l.Unlock()
				return g.acquire(context.Background(), "svc", false, nil)
			},
		},
		"replica/sin candado": {
			montar: func(g *Scheduler, e *entry) { g.extra["svc"] = []*entry{e} },
			adquirir: func(g *Scheduler) (*api.Machine, string, error) {
				return g.acquire(context.Background(), "svc", false, nil)
			},
		},
		"aislada/sesion": {
			montar: func(g *Scheduler, e *entry) {
				e.aislada = "k"
				g.extra["svc"] = []*entry{e}
				g.aisladas = map[string]*aislada{"k": {key: "k", service: "svc", machineID: "m1", lastUse: time.Now()}}
			},
			adquirir: func(g *Scheduler) (*api.Machine, string, error) {
				l := g.aisladaLock("k")
				l.Lock()
				defer l.Unlock()
				return g.despertarAislada(context.Background(), "m1", &WakeTrace{})
			},
		},
	}
	for nombre, c := range casos {
		t.Run(nombre, func(t *testing.T) {
			mc := instanciaVieja("m1")
			mc.State = api.StateRunning
			d := &daemonFalso{maquinas: map[string]*api.Machine{"m1": mc}}
			g := conDaemonFalso(t, d)
			dentro, soltar := make(chan struct{}), make(chan struct{})
			g.freezeFn = func(id string) error {
				close(dentro)
				<-soltar
				_, err := g.client.Freeze(context.Background(), id)
				return err
			}
			c.montar(g, &entry{machineID: "m1", ip: mc.IP, lastUse: time.Now().Add(-time.Hour), renewedAt: time.Now()})

			fin := make(chan struct{})
			go func() { defer close(fin); g.reapOnce(context.Background()) }()
			<-dentro

			type res struct {
				mc  *api.Machine
				how string
				err error
			}
			hecho := make(chan res, 1)
			go func() {
				mc, how, err := c.adquirir(g)
				hecho <- res{mc, how, err}
			}()
			// Sin el arreglo el acquire acaba aquí mismo, adoptándola.
			select {
			case r := <-hecho:
				hecho <- r
			case <-time.After(300 * time.Millisecond):
			}
			close(soltar)
			<-fin
			r := <-hecho

			if r.err == nil && r.mc != nil && r.mc.ID == "m1" {
				if st := d.estado("m1"); st != api.StateRunning {
					t.Fatalf("se adoptó m1 (%s) y el freeze del segador la dejó %q: la primera petición daría 502", r.how, st)
				}
			}
			if n := d.visto("POST /machines/m1/freeze"); n != 1 {
				t.Errorf("m1 debía congelarse una vez (%d)", n)
			}
			g.mu.Lock()
			marcada := g.adquiriendo["m1"]
			g.mu.Unlock()
			if marcada && (r.mc == nil || r.mc.ID != "m1") {
				t.Error("m1 quedó marcada en adquiriendo sin que nadie la adquiriera")
			}
		})
	}
}

// Si el candado de quien la despertaría está tomado, el segador no la toca en
// esta vuelta: alguien la está usando ahora mismo.
func TestElSegadorRespetaElCandadoDeQuienLaDespierta(t *testing.T) {
	g := New(nil, time.Minute, false, 0)
	congeladas := 0
	g.freezeFn = func(string) error { congeladas++; return nil }
	e := &entry{machineID: "m1", lastUse: time.Now().Add(-time.Hour), renewedAt: time.Now()}
	g.services["svc"] = e
	l := g.ensureLock("svc")
	l.Lock()
	g.reapOnce(context.Background())
	l.Unlock()
	if congeladas != 0 {
		t.Fatalf("se congeló con el candado del servicio tomado")
	}
	g.mu.Lock()
	sigue := g.services["svc"] == e
	g.mu.Unlock()
	if !sigue {
		t.Fatal("la instancia salió del mapa sin congelarse")
	}
	g.reapOnce(context.Background())
	if congeladas != 1 {
		t.Fatalf("con el candado libre debía congelarse (%d)", congeladas)
	}
}

// Un freeze fallido devuelve la instancia al mapa, como en evictLRU: fuera de
// él, el gateway la cree inexistente y un acquire la adopta cuando ya no toca.
func TestElSegadorReponeLaQueNoPudoCongelar(t *testing.T) {
	g := New(nil, time.Minute, false, 0)
	g.freezeFn = func(string) error { return context.DeadlineExceeded }
	e := &entry{machineID: "m1", lastUse: time.Now().Add(-time.Hour), renewedAt: time.Now()}
	g.services["svc"] = e
	g.reapOnce(context.Background())
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.services["svc"] != e {
		t.Fatal("la instancia que no se pudo congelar no volvió al mapa")
	}
	if g.adquiriendo["m1"] {
		t.Fatal("quedó marcada en adquiriendo")
	}
}
