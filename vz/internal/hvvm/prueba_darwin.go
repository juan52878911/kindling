//go:build darwin && arm64

package hvvm

/*
#include <stdlib.h>
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

// vsockReuso cruza dos dials de vsock en un mismo slot sin invitado
// (khv_vsock_prueba_reuso): devuelve lo que vio el primero y si el segundo
// recibió el mismo número de fd.
func vsockReuso() (ret int, mismoFD bool) {
	var c C.int
	ret = int(C.khv_vsock_prueba_reuso(&c))
	return ret, c != 0
}

// vsockCierre: una conexión que el invitado medio cerró y Go cierra del todo
// (medio=false) o solo por su lado de escritura (medio=true); devuelve si una
// pasada de poll pidió al invitado el cierre completo (khv_vsock_prueba_cierre).
func vsockCierre(medio bool) int {
	m := C.int(0)
	if medio {
		m = 1
	}
	return int(C.khv_vsock_prueba_cierre(m))
}

// carreraCola atiende iters lecturas del disco mientras otro hilo cambia el
// QueueNum (prueba.c): devuelve las respondidas, -1 si no se pudo preparar y
// -2 si el QueueNum cambió con la cola lista.
func carreraCola(iters int) int { return int(C.khv_prueba_carrera(C.int(iters))) }

// flushDisco manda un FLUSH al disco de pruebas y devuelve su byte de estado
// (0 = bien), o -1 si no se pudo preparar.
func flushDisco() int { return int(C.khv_prueba_flush()) }

// construirFDT llama a fdt_build con una VM de mentira (2 vCPU, 256 MiB, un
// virtio) y devuelve el DTB, o nil si no se pudo construir.
func construirFDT() []byte {
	buf := make([]byte, 1<<16)
	args := C.CString("console=ttyS0")
	defer C.free(unsafe.Pointer(args))
	p := C.struct_fdt_params{ram_size: 256 << 20, ncpus: 2, bootargs: args, dist_size: 0x10000,
		redist_size: 0x20000, uart_spi: 33, rtc_spi: 34, nvirtio: 1}
	p.virtio_spi[0] = 35
	n := C.fdt_build(unsafe.Pointer(&buf[0]), C.size_t(len(buf)), &p)
	if n == 0 {
		return nil
	}
	return buf[:n]
}
