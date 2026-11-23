package scheduler

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// B-02: el segador tomaba los candados de TODAS las víctimas de la vuelta y
// las congelaba en serie, soltando cada candado solo al acabar su freeze. Una
// petición a un servicio que aún no le tocaba esperaba detrás de los freezes
// de los demás. Cada víctima se aparta justo antes de dormirla.
func TestElSegadorNoRetieneElCandadoDeLasQueAunNoCongela(t *testing.T) {
	g := New(nil, time.Minute, false, 0)
	dentro, soltar := make(chan string, 1), make(chan struct{})
	var once sync.Once
	g.freezeFn = func(id string) error {
		primera := false
		once.Do(func() { primera = true })
		if primera {
			dentro <- id
			<-soltar
		}
		return nil
	}
	viejo := time.Now().Add(-time.Hour)
	g.services["a"] = &entry{machineID: "m-a", lastUse: viejo, renewedAt: time.Now()}
	g.services["b"] = &entry{machineID: "m-b", lastUse: viejo, renewedAt: time.Now()}

	fin := make(chan struct{})
	go func() { defer close(fin); g.reapOnce(context.Background()) }()
	id := <-dentro
	otro := "a"
	if id == "m-a" {
		otro = "b"
	}
	l := g.ensureLock(otro)
	libre := l.TryLock()
	if libre {
		l.Unlock()
	}
	close(soltar)
	<-fin
	if !libre {
		t.Fatalf("el candado de %q estaba tomado mientras se congelaba otra (%s)", otro, id)
	}
}

// B-02: el freeze del segador iba sin plazo con el ctx de Reap, que no vence:
// un freeze atascado en el daemon paraba el segador para siempre con los
// candados tomados (y, con él, el latido del arrendamiento).
func TestElSegadorAcotaUnFreezeAtascado(t *testing.T) {
	antes := plazoDormir
	plazoDormir = 200 * time.Millisecond
	t.Cleanup(func() { plazoDormir = antes })

	mc := instanciaVieja("m1")
	mc.State = api.StateRunning
	d := &daemonFalso{maquinas: map[string]*api.Machine{"m1": mc}}
	atascado := make(chan struct{})
	t.Cleanup(func() { close(atascado) })
	g := gwConManejador(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/freeze") {
			select {
			case <-atascado:
			case <-r.Context().Done():
			}
			return
		}
		d.ServeHTTP(w, r)
	}))
	e := &entry{machineID: "m1", ip: mc.IP, lastUse: time.Now().Add(-time.Hour), renewedAt: time.Now()}
	g.services["svc"] = e

	fin := make(chan struct{})
	go func() { defer close(fin); g.reapOnce(context.Background()) }()
	select {
	case <-fin:
	case <-time.After(10 * time.Second):
		t.Fatal("reapOnce sigue esperando a un freeze atascado")
	}
	l := g.ensureLock("svc")
	if !l.TryLock() {
		t.Fatal("el candado del servicio quedó tomado")
	}
	l.Unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.services["svc"] != e {
		t.Error("la instancia que no se pudo congelar no volvió al mapa")
	}
	if g.adquiriendo["m1"] {
		t.Error("quedó marcada en adquiriendo")
	}
}

// B-02: un pánico en OnSleep (o al dormir) lo recoge panico.Contener en Reap,
// pero los candados y las marcas con que se apartaron las víctimas se soltaban
// solo en el callback de su freeze: quedaban tomados para siempre y el
// servicio no volvía a despertar.
func TestElSegadorSueltaLoApartadoSiHayPanico(t *testing.T) {
	g := New(nil, time.Minute, false, 0)
	g.freezeFn = func(string) error { return nil }
	g.OnSleep = func(string, string) { panic("fallo de prueba") }
	e := &entry{machineID: "m1", lastUse: time.Now().Add(-time.Hour), renewedAt: time.Now()}
	g.services["svc"] = e

	func() {
		defer func() { _ = recover() }()
		g.reapOnce(context.Background())
	}()

	l := g.ensureLock("svc")
	if !l.TryLock() {
		t.Fatal("el candado del servicio quedó tomado tras el pánico")
	}
	l.Unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.adquiriendo["m1"] {
		t.Error("quedó marcada en adquiriendo tras el pánico")
	}
	if g.services["svc"] != e {
		t.Error("la instancia apartada no volvió al mapa tras el pánico")
	}
}
