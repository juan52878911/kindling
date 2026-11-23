package machine

import (
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// discosMaxHV es KHV_MAX_DISKS de kling-hv (vz/internal/hvvm/khv.h): el VMM
// rechaza el noveno disco al crear la VM (hvvm_darwin.go).
const discosMaxHV = 8

// Lo más que monta el daemon en una máquina es rootfs + overlay, los volúmenes
// (api.MaxVolumes) y la capa: si eso deja de caber en los discos de kling-hv,
// una petición válida fallaría tarde, al crear la VM. Este test lo para antes.
func TestLosDiscosDelDaemonCabenEnHV(t *testing.T) {
	if n := 2 + api.MaxVolumes + 1; n > discosMaxHV {
		t.Fatalf("el daemon puede montar %d discos y kling-hv admite %d: valida len(drives) al pedir la máquina", n, discosMaxHV)
	}
}
