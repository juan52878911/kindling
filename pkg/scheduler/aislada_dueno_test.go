package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// B-07: el barrido de aisladas huérfanas solo filtraba por MachineLabels, que
// comparten todos los gateways del mismo producto. Dos gateways sobre un
// daemon se tomaban por huérfanas las microVM de sesión del otro y las
// destruían. Como el fondo (PoolOwner), cada aislada lleva el id estable de
// quien la creó y solo ese la barre.
func TestBarridoNoSeLlevaLasAisladasDeOtroGateway(t *testing.T) {
	g1, d := conDaemonAislado(t)
	ctx := context.Background()
	g1.PoolOwner = "gw-1"
	a, err := g1.isolatedSession(ctx, "svc", "A", sinTenant, true)
	if err != nil {
		t.Fatalf("isolatedSession: %v", err)
	}
	vieja := time.Now().Add(-time.Hour)
	d.mu.Lock()
	if got := d.maquinas[a.machineID].Labels[LabelIsolatedOwner]; got != "gw-1" {
		t.Errorf("la aislada nueva lleva dueño %q; quería gw-1", got)
	}
	d.maquinas[a.machineID].CreatedAt = vieja
	// Una de antes de la etiqueta de dueño: no se sabe de quién es.
	d.maquinas["sin-dueno"] = &api.Machine{ID: "sin-dueno", State: api.StateWarm, CreatedAt: vieja,
		Labels: map[string]string{LabelIsolated: "true"}}
	// Una huérfana de verdad del segundo gateway.
	d.maquinas["propia"] = &api.Machine{ID: "propia", State: api.StateWarm, CreatedAt: vieja,
		Labels: map[string]string{LabelIsolated: "true", LabelIsolatedOwner: "gw-2"}}
	d.mu.Unlock()

	g2 := New(g1.client, time.Minute, false, 0)
	g2.PoolOwner = "gw-2"
	g2.barrerAisladas(ctx)

	if !d.existe(a.machineID) {
		t.Error("el segundo gateway destruyó la microVM de sesión del primero")
	}
	if !d.existe("sin-dueno") {
		t.Error("se barrió una aislada sin dueño conocido")
	}
	if d.existe("propia") {
		t.Error("la huérfana propia del segundo gateway sigue")
	}
}

// Sin id estable no se sabe qué es de quién: no se barre nada.
func TestBarridoSinDuenoNoBarre(t *testing.T) {
	g, d := conDaemonAislado(t)
	ctx := context.Background()
	d.mu.Lock()
	d.maquinas["huerfana"] = &api.Machine{ID: "huerfana", State: api.StateWarm, CreatedAt: time.Now().Add(-time.Hour),
		Labels: map[string]string{LabelIsolated: "true"}}
	d.mu.Unlock()
	g.barrerAisladas(ctx)
	if !d.existe("huerfana") {
		t.Error("sin PoolOwner el barrido destruyó una aislada")
	}
}
