//go:build darwin && arm64

package hvvm

import "testing"

// El vCPU i tiene que ser el i-ésimo que se crea en HVF: el estado del GIC
// guarda los redistribuidores por orden de creación, y con los hilos creando
// a la vez una restauración podía darle a cada vCPU el del otro (vm.c,
// turno_esperar).
func TestVCPUsSeCreanPorOrden(t *testing.T) {
	for i := 0; i < 5; i++ {
		if r := pruebaTurnoVCPUs(); r != 0 {
			t.Fatalf("round %d: vCPU creation order broken at position %d", i, r-1)
		}
	}
}
