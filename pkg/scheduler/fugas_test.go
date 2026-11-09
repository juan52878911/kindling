package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// pausadaQueNoDespierta monta m1 pausada en el daemon falso y en el registro,
// con un thaw que falla siempre.
func pausadaQueNoDespierta(t *testing.T, aislada string) (*Scheduler, *daemonFalso) {
	t.Helper()
	mc := instanciaVieja("m1")
	mc.State = api.StatePaused
	d := &daemonFalso{
		maquinas:   map[string]*api.Machine{"m1": mc},
		fallarThaw: map[string]int{"m1": http.StatusInternalServerError},
	}
	g := conDaemonFalso(t, d)
	g.pausadas = map[string]pausada{"m1": {service: "svc", memMiB: 64, at: time.Now(), aislada: aislada}}
	return g, d
}

// Si el thaw de una pausada falla, sigue pausada en el daemon: tiene que
// seguir en g.pausadas, o nadie la congelaría nunca (RAM sin dueño). Por los
// dos caminos que la reanudan: acquire y el despertar de una sesión aislada.
func TestThawFallidoNoPierdeLaPausada(t *testing.T) {
	caminos := map[string]func(g *Scheduler) error{
		"acquire": func(g *Scheduler) error {
			_, _, err := g.acquire(context.Background(), "svc", false, &WakeTrace{})
			return err
		},
		"aislada": func(g *Scheduler) error {
			_, _, err := g.despertarAislada(context.Background(), "m1", &WakeTrace{})
			return err
		},
	}
	for nombre, despertar := range caminos {
		t.Run(nombre, func(t *testing.T) {
			aislada := ""
			if nombre == "aislada" {
				aislada = "k"
			}
			g, d := pausadaQueNoDespierta(t, aislada)
			if err := despertar(g); err == nil {
				t.Fatal("el thaw falla y el despertar no dio error")
			}
			if d.visto("POST /machines/m1/thaw") != 1 {
				t.Fatalf("no se intentó el thaw (%d)", d.visto("POST /machines/m1/thaw"))
			}
			g.mu.Lock()
			_, sigue := g.pausadas["m1"]
			marcada := g.adquiriendo["m1"]
			g.mu.Unlock()
			if !sigue {
				t.Error("el thaw falló y m1, aún pausada en el daemon, salió del registro de pausadas")
			}
			if marcada {
				t.Error("m1 quedó marcada en adquiriendo")
			}
		})
	}
}

// Un Remove fallido no cuenta como desalojo: la máquina sigue viva. evictOne
// lo dice (false), y la máquina no se olvida: evictStale y drain la vuelven a
// intentar hasta borrarla.
func TestElFondoNoDaPorBorradaUnaQueSigueViva(t *testing.T) {
	var mu sync.Mutex
	fallar := true
	vivas := map[string]bool{"a-vieja": true, "a-nueva": true}
	p := &pool{
		size:  2,
		ready: map[string][]*warmVM{},
		removeFn: func(_ context.Context, id string) error {
			mu.Lock()
			defer mu.Unlock()
			if fallar {
				return errors.New("daemon busy")
			}
			delete(vivas, id)
			return nil
		},
	}
	ahora := time.Now()
	p.ready["a"] = []*warmVM{{id: "a-vieja", born: ahora.Add(-time.Hour)}, {id: "a-nueva", born: ahora}}

	if p.evictOne(t.Context()) {
		t.Fatal("evictOne dio por retirada una máquina cuyo Remove falló")
	}
	// Ya no se entrega a nadie: se iba a destruir.
	if vm := p.take("a"); vm == nil || vm.id != "a-nueva" {
		t.Fatalf("take = %+v; quería a-nueva (a-vieja ya salió del fondo)", vm)
	}

	mu.Lock()
	fallar = false
	mu.Unlock()
	p.evictStale(t.Context(), time.Hour*24)
	mu.Lock()
	defer mu.Unlock()
	if vivas["a-vieja"] {
		t.Error("el Remove fallido no se reintentó: a-vieja sigue viva sin que nadie la recuerde")
	}
}

// drain también reintenta las pendientes: al apagar no puede quedar ninguna.
func TestDrainReintentaLasPendientes(t *testing.T) {
	falladas := 0
	var quitadas []string
	p := &pool{
		size:  1,
		ready: map[string][]*warmVM{},
		removeFn: func(_ context.Context, id string) error {
			if falladas == 0 {
				falladas++
				return errors.New("daemon busy")
			}
			quitadas = append(quitadas, id)
			return nil
		},
	}
	p.ready["a"] = []*warmVM{{id: "m1", born: time.Now().Add(-time.Hour)}}
	p.evictStale(t.Context(), time.Minute)
	p.drain(t.Context())
	if len(quitadas) != 1 || quitadas[0] != "m1" {
		t.Fatalf("quitadas = %v; quería [m1] reintentada en drain", quitadas)
	}
}

// Una llamada más larga que idle deja vieja la ruta de su sesión (lastUse es
// cuándo llegó), pero sigue en vuelo: el segador no puede borrarla, o la
// siguiente petición de esa sesión daría 404.
func TestElSegadorNoBorraLaRutaDeUnaLlamadaEnVuelo(t *testing.T) {
	g := &Scheduler{
		idle:     time.Minute,
		services: map[string]*entry{},
		extra:    map[string][]*entry{},
		routes:   map[string]*sessionRoute{},
	}
	hace := time.Now().Add(-5 * time.Minute)
	g.services["svc"] = &entry{machineID: "m1", lastUse: hace, renewedAt: time.Now(), inflight: 1}
	g.routes["larga"] = &sessionRoute{service: "svc", machineID: "m1", lastUse: hace}
	g.routes["ociosa"] = &sessionRoute{service: "svc", machineID: "m-otra", lastUse: hace}

	g.reapOnce(context.Background())

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.routes["larga"] == nil {
		t.Error("el segador borró la ruta de una sesión con una llamada en vuelo")
	}
	if g.routes["ociosa"] != nil {
		t.Error("la ruta ociosa de verdad debía caducar")
	}
}

// N initialize concurrentes contra instancias de una sesión: cada uno tiene
// que acabar en una instancia distinta. Sin reservar el hueco al elegir, todos
// leían "0 sesiones < 1" de la misma y el puente rechazaba a N-1.
func TestPickInstanceReservaElHueco(t *testing.T) {
	const n = 8
	var creadas atomic.Int64
	g := &Scheduler{
		services:    map[string]*entry{},
		extra:       map[string][]*entry{},
		routes:      map[string]*sessionRoute{},
		MaxReplicas: n,
	}
	g.services["svc"] = &entry{machineID: "m-0", maxSessions: 1, checkedAt: time.Now(), lastUse: time.Now()}
	g.buildFn = func(context.Context, string, *tenant, bool) (*entry, error) {
		id := creadas.Add(1)
		time.Sleep(5 * time.Millisecond) // lo que tarda un restore
		return &entry{machineID: fmt.Sprintf("m-%d", id), maxSessions: 1, lastUse: time.Now()}, nil
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	por := map[string]int{}
	inicio := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-inicio
			e, err := g.pickInstance(context.Background(), "svc", tenantFrom(context.Background()))
			if err != nil {
				t.Errorf("pickInstance: %v", err)
				return
			}
			mu.Lock()
			por[e.machineID]++
			mu.Unlock()
		}()
	}
	close(inicio)
	wg.Wait()
	for id, k := range por {
		if k > 1 {
			t.Errorf("%d initialize concurrentes fueron a %s, que solo admite una sesión", k, id)
		}
	}

	// La reserva se suelta al acabar la petición: la instancia vuelve a
	// admitir una sesión si el initialize no llegó a fijarla.
	e := g.services["svc"]
	g.begin(e)
	g.end(e)
	if got, err := g.pickInstance(context.Background(), "svc", tenantFrom(context.Background())); err != nil || got != e {
		t.Errorf("tras soltar la reserva, pickInstance = %v, %v; quería la primaria", got, err)
	}
}

// Una máquina despierta cuya herramienta no llega a escuchar no se queda viva
// sin dueño: con MachineTTL negativo nadie más la pararía. La recién
// restaurada se borra; la adoptada o descongelada se congela.
func TestMaquinaQueNoEscuchaNoQuedaSinDueno(t *testing.T) {
	for _, how := range []string{"restore", "thaw", "adopt"} {
		t.Run(how, func(t *testing.T) {
			mc := instanciaVieja("m1")
			mc.State = api.StateRunning
			// Con reenvíos esperarListo pregunta al daemon, y el falso no
			// sabe sondear: falla al momento, como una herramienta muda.
			mc.Forwards = map[string]string{"8080": "127.0.0.1:1"}
			d := &daemonFalso{maquinas: map[string]*api.Machine{"m1": mc}}
			g := conDaemonFalso(t, d)
			g.MachineTTL = -1
			var borradas []string
			g.removeFn = func(_ context.Context, id string) error { borradas = append(borradas, id); return nil }

			_, err := g.buildEntryWith(context.Background(), "svc", tenantFrom(context.Background()),
				func(*WakeTrace) (*api.Machine, string, error) {
					g.mu.Lock()
					if g.adquiriendo == nil {
						g.adquiriendo = map[string]bool{}
					}
					g.adquiriendo["m1"] = true
					g.mu.Unlock()
					c := *mc
					return &c, how, nil
				})
			if err == nil {
				t.Fatal("la herramienta no escucha y buildEntry no dio error")
			}
			switch how {
			case "restore":
				if len(borradas) != 1 || borradas[0] != "m1" {
					t.Errorf("la restaurada que no escucha no se borró (borradas %v)", borradas)
				}
			default:
				if st := d.estado("m1"); st != api.StateWarm {
					t.Errorf("la %s que no escucha quedó %q; quería congelada", how, st)
				}
				if len(borradas) != 0 {
					t.Errorf("se borró una máquina %s que guarda estado (%v)", how, borradas)
				}
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.adquiriendo["m1"] {
				t.Error("m1 quedó marcada en adquiriendo")
			}
		})
	}
}
