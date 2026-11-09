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
