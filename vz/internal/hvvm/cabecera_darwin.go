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
// versión de la cabecera, su tamaño, el de la versión 1 y el del pie.
func formatoSnap() (magia uint64, version uint32, hdr, hdrV1, pie uint64) {
	var m, h, h1, p C.uint64_t
	var v C.uint32_t
	C.khv_formato_snap(&m, &v, &h, &h1, &p)
	return uint64(m), uint32(v), uint64(h), uint64(h1), uint64(p)
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
