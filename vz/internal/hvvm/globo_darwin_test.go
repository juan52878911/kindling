//go:build darwin && arm64

package hvvm

import "testing"

// El driver de Linux entrega los marcos de arriba abajo, 256 por búfer: el
// globo soltaba una página del host por tramo (un hv_vm_unmap, un mmap y un
// hv_vm_map cada una; medido en el M4, ~5300 tramos por squeeze de 80 MiB).
// Ahora junta las páginas enteras y las suelta en tramos contiguos. El
// informe de páginas libres llega desordenado y también sale en un tramo, y
// se devuelve al driver (que no reutiliza esas páginas hasta entonces).
func TestGloboJuntaTramos(t *testing.T) {
	inflar, informe, devueltos, desinflar, ok := pruebaGlobo()
	if !ok {
		t.Fatal("khv_prueba_globo: no fake RAM")
	}
	if inflar != 1 {
		t.Errorf("inflating 32 contiguous host pages (descending PFNs): %d runs, want 1", inflar)
	}
	if informe != 1 {
		t.Errorf("free page report of two adjacent blocks out of order: %d runs, want 1", informe)
	}
	if devueltos != 1 {
		t.Errorf("free page report: %d buffers returned to the driver, want 1", devueltos)
	}
	if desinflar != 0 {
		t.Errorf("deflating: %d runs released, want 0 (nothing to release)", desinflar)
	}
}
