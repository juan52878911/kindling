//go:build darwin && arm64

package hvvm

/*
#include <stdlib.h>
#include "khv.h"
*/
import "C"

import (
	"errors"
	"unsafe"
)

// formatoSnap es la disposición del mem.file de kling-hv (vm.c): la magia, la
// versión más nueva que escribe, el tamaño de la cabecera de la 4 (y de las
// 1, 2 y 3), el del pie, el de un vCPU (cpu_state), cuántos caben en la de la
// 4 (la 5 lleva detrás de esa los demás) y, de la sección de virtio-fs de la
// 6, el tamaño de su cabecera y el de cada dispositivo.
func formatoSnap() (magia uint64, version uint32, hdr, hdrV1, hdrV2, hdrV3, pie, cpu uint64, cpusV4 uint32, vfsCab, vfs uint64) {
	var m, h, h1, h2, h3, p, c, vc, vd C.uint64_t
	var v, n C.uint32_t
	C.khv_formato_snap(&m, &v, &h, &h1, &h2, &h3, &p, &c, &n, &vc, &vd)
	return uint64(m), uint32(v), uint64(h), uint64(h1), uint64(h2), uint64(h3), uint64(p), uint64(c), uint32(n), uint64(vc), uint64(vd)
}

// restaurarSinVM llama a khv_restore sobre path sin VM creada. Solo para los
// tests de la cabecera: lo primero que hace es comprobarla, y con una que
// pase falla al comparar con la máquina configurada (no hay), antes de tocar
// nada del framework.
func restaurarSinVM(path string) error {
	p := C.CString(path)
	defer C.free(unsafe.Pointer(p))
	if C.khv_restore(p) != 0 {
		return errors.New(C.GoString(C.khv_error()))
	}
	return nil
}
