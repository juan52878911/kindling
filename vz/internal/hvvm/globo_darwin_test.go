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

// TestGloboCuotaInforme: un invitado que informa una y otra vez del mismo
// bloque libre hacía remapear su RAM a voluntad (hv_vm_unmap, mmap y
// hv_vm_map por informe, bajo el cerrojo que también toman las estadísticas
// y la pausa). El informe tiene una cuota por segundo: lo que pasa de ella se
// devuelve sin soltar.
func TestGloboCuotaInforme(t *testing.T) {
	const n = 2000
	tramos, devueltos, ok := pruebaGloboCuota(n)
	if !ok {
		t.Fatal("khv_prueba_globo_cuota: no fake RAM")
	}
	if devueltos != n {
		t.Errorf("%d reports: %d buffers returned to the driver, want all", n, devueltos)
	}
	if tramos == 0 || tramos > n/2 {
		t.Errorf("%d reports of the same block in a burst: %d runs released, want some but at most %d", n, tramos, n/2)
	}
}
