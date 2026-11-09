//go:build darwin && arm64

package hvvm

import (
	"errors"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/vz/internal/server"
)

// HV_NO_RESOURCES (la VM 128 del Mac) queda marcado en vm.c y el error de
// Create envuelve server.ErrSinCapacidad, que el servidor contesta con 507,
// con un mensaje que dice qué pasa y qué hacer. Otro fallo de HVF no.
func TestCreateSinRecursosEsDeCapacidad(t *testing.T) {
	const hvNoResources, hvBusy = 0xfae94005, 0xfae94002
	if !pruebaSinRecursos(hvNoResources) {
		t.Fatal("HV_NO_RESOURCES de un TRY no marca khv_sin_recursos")
	}
	if pruebaSinRecursos(hvBusy) {
		t.Fatal("HV_BUSY marca khv_sin_recursos")
	}
	err := errCreate(true, "hv_vm_create(NULL) failed: 0xfae94005 (HV_NO_RESOURCES)")
	if !errors.Is(err, server.ErrSinCapacidad) {
		t.Fatalf("%v no es server.ErrSinCapacidad", err)
	}
	for _, s := range []string{"creating the VM", "0xfae94005", "127", "kling ps"} {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("%q no dice %q", err, s)
		}
	}
	if err := errCreate(false, "hv_gic_create(g) failed: 0xfae94001"); errors.Is(err, server.ErrSinCapacidad) {
		t.Fatalf("%v es de capacidad", err)
	}
}
