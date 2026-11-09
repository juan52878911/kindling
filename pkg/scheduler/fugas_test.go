package scheduler

import (
	"context"
	"net/http"
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
