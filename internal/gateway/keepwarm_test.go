package gateway

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
	g := &Gateway{
		services: map[string]*entry{},
		extra:    map[string][]*entry{},
		routes:   map[string]*sessionRoute{},
		// KeepWarm se queda en su cero por defecto; client a nil a propósito.
	}
	// No debe entrar en pánico ni bloquear: retorna de inmediato por el corte.
	g.KeepWarmAll(context.Background())
}
