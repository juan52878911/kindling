package scheduler

import (
	"context"
	"testing"
)

// El keep-warm está apagado por defecto (KeepWarm == 0) y esa garantía es lo que lo
// hace seguro en hosts con la RAM justa (la caja x86): no debe siquiera hablar con el
// daemon. Se prueba con el cliente a nil: si KeepWarmAll intentara listar snapshots,
// entraría en g.client.Snapshots y provocaría un panic. Que no lo haga demuestra que
// el corte ocurre antes de tocar nada.
func TestKeepWarmAll_DesactivadoNoTocaElDaemon(t *testing.T) {
	g := &Scheduler{
		services: map[string]*entry{},
		extra:    map[string][]*entry{},
		routes:   map[string]*sessionRoute{},
		// KeepWarm se queda en su cero por defecto; client a nil a propósito.
	}
	// No debe entrar en pánico ni bloquear: retorna de inmediato por el corte.
	g.KeepWarmAll(context.Background())
}

// G-05: mientras un ensure lanzado por KeepWarmAll sigue en vuelo, una pasada
// siguiente del mismo servicio no debe apilar otro. keepwarmEnVuelo/
// keepwarmListo son el candado de esa reserva; se prueban directamente porque
// KeepWarmAll en sí necesita un daemon (Snapshots) para tener algo que hacer.
func TestKeepwarmEnVueloNoApilaEnsures(t *testing.T) {
	g := &Scheduler{}

	if !g.keepwarmEnVuelo("svc") {
		t.Fatal("la primera reserva de 'svc' debía conseguirse")
	}
	if g.keepwarmEnVuelo("svc") {
		t.Fatal("una segunda pasada no debe poder apilar otro ensure del mismo servicio")
	}
	// Otro servicio no se ve afectado por la reserva de "svc".
	if !g.keepwarmEnVuelo("otro") {
		t.Fatal("un servicio distinto no debía chocar con la reserva de 'svc'")
	}

	g.keepwarmListo("svc")
	if !g.keepwarmEnVuelo("svc") {
		t.Fatal("liberada la reserva, la siguiente pasada debe poder volver a intentarlo")
	}
}
