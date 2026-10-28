//go:build darwin

package vzvm

import (
	"testing"

	"github.com/Code-Hex/vz/v3"

	"github.com/juan52878911/kindling/vz/internal/spec"
)

// Los modos de cada disco: la base y las capas (solo lectura) con caché del
// host, el overlay desechable sin sincronizar, todo lo demás como siempre, y
// las variables de entorno mandan sobre todo.
func TestModosDisco(t *testing.T) {
	base := spec.Drive{DriveID: "rootfs", IsRootDevice: true, IsReadOnly: true}
	overlay := spec.Drive{DriveID: "overlay"}
	efimero := spec.Drive{DriveID: "overlay", Disposable: true}
	// Marcado desechable pero de solo lectura o raíz: no cuenta.
	raizMarcada := spec.Drive{DriveID: "rootfs", IsRootDevice: true, Disposable: true}
	for _, tc := range []struct {
		nombre     string
		d          spec.Drive
		envC, envS string
		cm         vz.DiskImageCachingMode
		sm         vz.DiskImageSynchronizationMode
		def        bool
	}{
		{"base", base, "", "", vz.DiskImageCachingModeCached, vz.DiskImageSynchronizationModeFull, false},
		{"overlay persistente", overlay, "", "", vz.DiskImageCachingModeAutomatic, vz.DiskImageSynchronizationModeFull, true},
		{"overlay desechable", efimero, "", "", vz.DiskImageCachingModeAutomatic, vz.DiskImageSynchronizationModeNone, false},
		{"raíz marcada", raizMarcada, "", "", vz.DiskImageCachingModeAutomatic, vz.DiskImageSynchronizationModeFull, true},
		{"env caché en la base", base, "uncached", "", vz.DiskImageCachingModeUncached, vz.DiskImageSynchronizationModeFull, false},
		{"env automatic en la base", base, "automatic", "", vz.DiskImageCachingModeAutomatic, vz.DiskImageSynchronizationModeFull, true},
		{"env sync en el desechable", efimero, "", "full", vz.DiskImageCachingModeAutomatic, vz.DiskImageSynchronizationModeFull, true},
		{"env none en el persistente", overlay, "", "none", vz.DiskImageCachingModeAutomatic, vz.DiskImageSynchronizationModeNone, false},
	} {
		cm, sm, def, err := modosDisco(tc.d, tc.envC, tc.envS)
		if err != nil {
			t.Fatalf("%s: %v", tc.nombre, err)
		}
		if cm != tc.cm || sm != tc.sm || def != tc.def {
			t.Errorf("%s: caché %v sync %v def %v, quería %v %v %v", tc.nombre, cm, sm, def, tc.cm, tc.sm, tc.def)
		}
	}
	if _, _, _, err := modosDisco(overlay, "rapido", ""); err == nil {
		t.Error("aceptó KLING_VZ_DISK_CACHING=rapido")
	}
	if _, _, _, err := modosDisco(overlay, "", "nunca"); err == nil {
		t.Error("aceptó KLING_VZ_DISK_SYNC=nunca")
	}
}
