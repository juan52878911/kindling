package machine

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/linkbroker"
)

// Pruebas de idle_freeze renovado por conexión (renovarPorUso, grafo_red.go).

// relojTTL es el TTLAt de la máquina id (cero si no lo tiene).
func relojTTL(m *Manager, id string) time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if mc := m.byID[id]; mc != nil && mc.TTLAt != nil {
		return *mc.TTLAt
	}
	return time.Time{}
}

// envejecerTTL deja la máquina id con idle_freeze 60 s, en el estado st y
// con el reloj puesto en viejo (vencido).
func envejecerTTL(m *Manager, id string, st api.State, viejo time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mc := m.byID[id]
	mc.TTLSeconds, mc.State = 60, st
	mc.TTLAt = &viejo
}

// idle_freeze es "N segundos sin conexiones": cada conexión que pasa la
// puerta reinicia el reloj del TTL del destino; una rechazada, no.
func TestGrafoConexionRenuevaIdleFreeze(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	g := e.montarGrafo(grafoTienda(false))
	web, apiID := e.maquina(g.ID, "web"), e.maquina(g.ID, "api")
	viejo := time.Now().Add(-time.Hour)
	vencida := func() bool { return slices.Contains(e.m.ttlVencidas(), apiID) }
	conectar := func(port int) error {
		_, _, err := e.m.resolverArista(context.Background(), web, g.ID, "web", "api", port, api.GraphEdgeLink)
		return err
	}

	// Rechazada (sin arista a ese puerto): el reloj no se mueve.
	envejecerTTL(e.m, apiID, api.StateRunning, viejo)
	if !vencida() {
		t.Fatal("api debería haber vencido")
	}
	if conectar(9090) == nil {
		t.Fatal("la conexión sin arista pasó")
	}
	if !relojTTL(e.m, apiID).Equal(viejo) || !vencida() {
		t.Fatalf("una conexión rechazada renovó el TTL: %v", relojTTL(e.m, apiID))
	}

	// Aceptada: el reloj vuelve a empezar y ya no vence.
	if err := conectar(8081); err != nil {
		t.Fatal(err)
	}
	if time.Since(relojTTL(e.m, apiID)) > time.Minute || vencida() {
		t.Fatalf("la conexión no renovó el TTL: %v", relojTTL(e.m, apiID))
	}

	// Una ráfaga no reescribe el reloj en cada conexión (como mucho una vez
	// por renovarMinimo).
	antes := relojTTL(e.m, apiID)
	if err := conectar(8081); err != nil {
		t.Fatal(err)
	}
	if !relojTTL(e.m, apiID).Equal(antes) {
		t.Fatal("dos conexiones seguidas reescribieron el reloj dos veces")
	}

	// Un destino congelado hace más de idle_freeze: despertarlo renueva, o
	// el vigilante lo volvería a congelar en su siguiente vuelta.
	envejecerTTL(e.m, apiID, api.StateWarm, viejo)
	if err := conectar(8081); err != nil {
		t.Fatal(err)
	}
	if time.Since(relojTTL(e.m, apiID)) > time.Minute || vencida() {
		t.Fatalf("el despertar no renovó el TTL: %v", relojTTL(e.m, apiID))
	}

	// Sin idle_freeze (TTL 0) no hay reloj que tocar.
	e.m.mu.Lock()
	e.m.byID[apiID].TTLSeconds, e.m.byID[apiID].TTLAt = 0, nil
	e.m.mu.Unlock()
	if err := conectar(8081); err != nil {
		t.Fatal(err)
	}
	if !relojTTL(e.m, apiID).IsZero() {
		t.Fatal("renovó una máquina sin TTL")
	}
}

// Un nodo que no corre no se renueva aunque alguien lo pida (el reloj de una
// máquina congelada es el de on_ttl, no el del uso).
func TestRenovarPorUsoSoloEnMarcha(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	g := e.montarGrafo(grafoTienda(false))
	apiID := e.maquina(g.ID, "api")
	viejo := time.Now().Add(-time.Hour)
	envejecerTTL(e.m, apiID, api.StateWarm, viejo)
	e.m.renovarPorUso(apiID)
	if !relojTTL(e.m, apiID).Equal(viejo) {
		t.Fatal("renovó una máquina congelada")
	}
	e.m.renovarPorUso("no-existe")
}

// El broker (macOS) renueva el idle_freeze del destino en cada conexión que
// entrega, y no en las que rechaza.
func TestBrokerRenuevaIdleFreeze(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	nuevoEcoBroker(t, e.m)
	g := e.montarGrafo(grafoTienda(false))
	web, apiID := e.maquina(g.ID, "web"), e.maquina(g.ID, "api")
	viejo := time.Now().Add(-time.Hour)
	envejecerTTL(e.m, apiID, api.StateRunning, viejo)

	if _, _, _, err := pedirBroker(t, e.m, web, linkbroker.Request{Kind: linkbroker.KindLink, Host: "api.graph", Port: 9090}); err == nil {
		t.Fatal("el broker entregó una arista que no existe")
	}
	if !relojTTL(e.m, apiID).Equal(viejo) {
		t.Fatal("una conexión rechazada renovó el TTL")
	}
	tc, _, _, err := pedirBroker(t, e.m, web, linkbroker.Request{Kind: linkbroker.KindLink, Host: "api.graph", Port: 8081})
	if err != nil {
		t.Fatal(err)
	}
	ecoPor(t, tc)
	if time.Since(relojTTL(e.m, apiID)) > time.Minute {
		t.Fatalf("la conexión entregada no renovó el TTL: %v", relojTTL(e.m, apiID))
	}
}
