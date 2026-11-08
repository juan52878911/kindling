package scheduler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// renovarTTL solo logueaba los fallos y el segador apuntaba renewedAt=now
// ANTES de renovar: una renovación fallida contaba como hecha y no se volvía a
// intentar hasta pasado otro umbral (idle/2), con el reloj del daemon
// corriendo. Ahora se reintenta en la vuelta siguiente.
func TestElSegadorReintentaUnaRenovacionFallida(t *testing.T) {
	mc := instanciaVieja("m1")
	mc.State = api.StateRunning
	d := &daemonFalso{maquinas: map[string]*api.Machine{"m1": mc},
		fallarRenew: map[string][]int{"m1": {http.StatusInternalServerError}}}
	g := conDaemonFalso(t, d)
	viejo := time.Now().Add(-time.Hour)
	e := &entry{machineID: "m1", lastUse: time.Now(), renewedAt: viejo}
	g.services["svc"] = e

	g.reapOnce(context.Background())
	g.mu.Lock()
	renovada := e.renewedAt
	g.mu.Unlock()
	if !renovada.Equal(viejo) {
		t.Fatal("la renovación falló y renewedAt se movió igual: se da por hecha")
	}
	g.reapOnce(context.Background())
	if n := d.visto("POST /machines/m1/renew"); n != 2 {
		t.Fatalf("renovaciones = %d; la fallida debía reintentarse en la vuelta siguiente", n)
	}
	g.mu.Lock()
	renovada = e.renewedAt
	g.mu.Unlock()
	if renovada.Equal(viejo) {
		t.Fatal("la renovación que sí salió no movió renewedAt")
	}
}

// Una renovación que el daemon rechaza porque la máquina ya no existe (404) o
// no está en marcha (409) no se arregla insistiendo: la entrada se descarta y
// el siguiente ensure la reconstruye (o la descongela).
func TestElSegadorDescartaLaEntradaSiLaMaquinaNoSePuedeRenovar(t *testing.T) {
	for _, c := range []struct {
		nombre string
		montar func(d *daemonFalso)
	}{
		{"no existe", func(d *daemonFalso) { delete(d.maquinas, "m1") }},
		{"no corre", func(d *daemonFalso) { d.fallarRenew = map[string][]int{"m1": {http.StatusConflict}} }},
	} {
		t.Run(c.nombre, func(t *testing.T) {
			mc := instanciaVieja("m1")
			mc.State = api.StateRunning
			d := &daemonFalso{maquinas: map[string]*api.Machine{"m1": mc}}
			c.montar(d)
			g := conDaemonFalso(t, d)
			g.services["svc"] = &entry{machineID: "m1", lastUse: time.Now(), renewedAt: time.Now().Add(-time.Hour)}
			g.routes["s1"] = &sessionRoute{service: "svc", machineID: "m1", lastUse: time.Now()}

			g.reapOnce(context.Background())
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.services["svc"] != nil {
				t.Fatal("la entrada sigue en el mapa con una máquina que no se puede renovar")
			}
		})
	}
}

// acquire adoptaba una en marcha aunque su renovación fallara. Con un 4xx no
// es adoptable (no corre de verdad, o ya no existe): se sigue buscando. Con un
// fallo del daemon se adopta, pero el segador la renueva en su vuelta
// siguiente en vez de fiarse del renewedAt de la entrada recién hecha.
func TestAcquireNoSeFiaDeUnaRenovacionFallida(t *testing.T) {
	t.Run("4xx", func(t *testing.T) {
		mc := instanciaVieja("m1")
		mc.State = api.StateRunning
		d := &daemonFalso{maquinas: map[string]*api.Machine{"m1": mc},
			fallarRenew: map[string][]int{"m1": {http.StatusConflict}}}
		g := conDaemonFalso(t, d)
		m, how, err := g.acquire(context.Background(), "svc", false, nil)
		if err == nil && m != nil && m.ID == "m1" {
			t.Fatalf("adoptó m1 (%s) con la renovación rechazada", how)
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.adquiriendo["m1"] {
			t.Fatal("m1 quedó marcada en adquiriendo")
		}
	})
	t.Run("5xx", func(t *testing.T) {
		mc := instanciaVieja("m1")
		mc.State = api.StateRunning
		d := &daemonFalso{maquinas: map[string]*api.Machine{"m1": mc},
			fallarRenew: map[string][]int{"m1": {http.StatusInternalServerError}}}
		g := conDaemonFalso(t, d)
		m, _, err := g.acquire(context.Background(), "svc", false, nil)
		if err != nil || m.ID != "m1" {
			t.Fatalf("acquire = %v, %v; con un fallo pasajero debía adoptarla", m, err)
		}
		// Como la registra buildEntryWith: renewedAt = ahora.
		g.mu.Lock()
		delete(g.adquiriendo, "m1")
		g.services["svc"] = &entry{machineID: "m1", lastUse: time.Now(), renewedAt: time.Now()}
		g.mu.Unlock()
		g.reapOnce(context.Background())
		if n := d.visto("POST /machines/m1/renew"); n != 2 {
			t.Fatalf("renovaciones = %d; el segador debía reintentar la que falló al adoptar", n)
		}
	})
}

// Lo mismo al despertar la máquina de una sesión aislada: si el daemon
// rechaza la renovación de una que el Get daba en marcha, no se adopta.
func TestDespertarAisladaNoAdoptaSiSeRechazaLaRenovacion(t *testing.T) {
	mc := instanciaVieja("m1")
	mc.State = api.StateRunning
	mc.Labels[LabelIsolated] = "true"
	d := &daemonFalso{maquinas: map[string]*api.Machine{"m1": mc},
		fallarRenew: map[string][]int{"m1": {http.StatusConflict}}}
	g := conDaemonFalso(t, d)
	if m, how, err := g.despertarAislada(context.Background(), "m1", &WakeTrace{}); err == nil {
		t.Fatalf("despertarAislada = %s (%s); con la renovación rechazada no debía adoptarla", m.ID, how)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.adquiriendo["m1"] {
		t.Fatal("m1 quedó marcada en adquiriendo")
	}
}
