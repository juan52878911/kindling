//go:build darwin && arm64

package hvvm

import "testing"

// Un vCPU que no volvió a correr entre dos pausas guarda en el snapshot el
// offset de la primera más el ajuste pendiente; sin él, el restaurado saltaba
// lo que duró la pausa anterior.
func TestVtimerPendienteEnElSnapshot(t *testing.T) {
	if r := pruebaVtimerPendiente(); r != 0 {
		t.Fatalf("khv_prueba_vtimer_pendiente failed check %d", r)
	}
}
