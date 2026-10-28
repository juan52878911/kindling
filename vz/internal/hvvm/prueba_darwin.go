//go:build darwin && arm64

package hvvm

/*
#include "khv.h"
*/
import "C"

import "unsafe"

// probarColas pasa data por los dispositivos virtio como lo haría un invitado
// hostil (prueba.c), sin VM. Solo para las pruebas: usa la RAM de mentira de
// prueba.c y no puede convivir con una VM de verdad en el mismo proceso.
func probarColas(data []byte) {
	if len(data) == 0 {
		return
	}
	C.khv_prueba((*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)))
}

// cadena llama a vq_cadena con una tabla de descriptores (addr, len, flags,
// next) y una cola de num entradas.
func cadena(descs [][4]uint64, head, num uint16, max int) int {
	tabla := make([]C.struct_vdesc, len(descs)+1)
	for i, d := range descs {
		tabla[i] = C.struct_vdesc{addr: C.uint64_t(d[0]), len: C.uint32_t(d[1]), flags: C.uint16_t(d[2]), next: C.uint16_t(d[3])}
	}
	q := C.struct_vq{num: C.uint32_t(num), ready: 1}
	out := make([]C.struct_vdesc, max+1)
	return int(C.vq_cadena(&q, &tabla[0], C.uint16_t(head), &out[0], C.int(max)))
}

// carreraCola atiende iters lecturas del disco mientras otro hilo cambia el
// QueueNum (prueba.c): devuelve las respondidas, -1 si no se pudo preparar y
// -2 si el QueueNum cambió con la cola lista.
func carreraCola(iters int) int { return int(C.khv_prueba_carrera(C.int(iters))) }

// flushDisco manda un FLUSH al disco de pruebas y devuelve su byte de estado
// (0 = bien), o -1 si no se pudo preparar.
func flushDisco() int { return int(C.khv_prueba_flush()) }
