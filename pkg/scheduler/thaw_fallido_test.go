package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// conListaOrdenada sirve d con GET /machines en el orden de ids: el de
// daemonFalso sale de un mapa y no deja fijar qué máquina se prueba primero.
func conListaOrdenada(t *testing.T, d *daemonFalso, ids ...string) *Scheduler {
	t.Helper()
	return gwConManejador(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/machines" {
			d.mu.Lock()
			l := []*api.Machine{}
			for _, id := range ids {
				c := *d.maquinas[id]
				l = append(l, &c)
			}
			d.mu.Unlock()
			_ = json.NewEncoder(w).Encode(l)
			return
		}
		d.ServeHTTP(w, r)
	}))
}

// B-06: un Thaw que fallaba devolvía el error tal cual y el siguiente acquire
// elegía la misma máquina: una instancia con el volcado dañado dejaba el
// servicio en 502 hasta que alguien la borrara. Ante un error que no es de
// capacidad ni del ctx, se descarta y se sigue con la siguiente.
func TestThawFallidoPruebaConLaSiguiente(t *testing.T) {
	casos := map[string]api.State{"pausada": api.StatePaused, "congelada": api.StateWarm}
	for nombre, estado := range casos {
		t.Run(nombre, func(t *testing.T) {
			rota := instanciaVieja("m1")
			rota.State = estado
			sana := instanciaVieja("m2")
			d := &daemonFalso{
				maquinas:   map[string]*api.Machine{"m1": rota, "m2": sana},
				fallarThaw: map[string]int{"m1": http.StatusInternalServerError},
			}
			g := conListaOrdenada(t, d, "m1", "m2")

			mc, how, err := g.acquire(context.Background(), "svc", false, &WakeTrace{})
			if err != nil {
				t.Fatalf("acquire: %v; debía descartar m1 y despertar m2", err)
			}
			if mc.ID != "m2" || how != "thaw" {
				t.Fatalf("acquire = %s (%s); quería m2 (thaw)", mc.ID, how)
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.adquiriendo["m1"] {
				t.Error("m1 quedó marcada en adquiriendo")
			}
		})
	}
}

// Falta de memoria no es una máquina rota: probar con otra pediría lo mismo.
// El error sube para que quien llama desaloje y reintente.
func TestThawSinMemoriaNoPruebaConOtra(t *testing.T) {
	rota := instanciaVieja("m1")
	sana := instanciaVieja("m2")
	d := &daemonFalso{
		maquinas:   map[string]*api.Machine{"m1": rota, "m2": sana},
		fallarThaw: map[string]int{"m1": api.StatusInsufficientMemory},
	}
	g := conListaOrdenada(t, d, "m1", "m2")

	if _, _, err := g.acquire(context.Background(), "svc", false, &WakeTrace{}); !api.IsInsufficientMemory(err) {
		t.Fatalf("acquire: %v; quería el error de memoria", err)
	}
	if n := d.visto("POST /machines/m2/thaw"); n != 0 {
		t.Errorf("se intentó despertar m2 (%d) tras quedarse sin memoria", n)
	}
}
