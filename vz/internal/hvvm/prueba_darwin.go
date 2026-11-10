//go:build darwin && arm64

package hvvm

/*
#include <stdlib.h>
#include "khv.h"
int khv_prueba_es(uint32_t tipo, uint64_t sector, const uint32_t *lens, int nd, uint8_t *datos, int malo,
                  uint32_t *usado);
uint32_t khv_prueba_cfg(uint32_t off);
int khv_prueba_used(void);
int khv_vsock_prueba_tx(uint8_t *out, uint32_t cap, const uint8_t *enviado, int *uso_pend);
int khv_prueba_tanda(bool pausa, uint64_t tope, int *estado);
int khv_vsock_prueba_fd_en_poll(void);
int khv_vsock_prueba_tanda_rx(bool pausa, uint64_t *lleno, uint64_t *movido, int64_t *ns);
*/
import "C"

import (
	"time"
	"unsafe"
)

// probarColas pasa data por los dispositivos virtio como lo haría un invitado
// hostil (prueba.c), sin VM. Solo para las pruebas: usa la RAM de mentira de
// prueba.c y no puede convivir con una VM de verdad en el mismo proceso.
// Devuelve el errno con el que habría muerto el hilo de recepción de la red
// (0 si sigue vivo).
func probarColas(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	r := int(C.khv_prueba((*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data))))
	if r < 0 {
		return 0 // no se pudo montar el banco: como antes, no es un fallo del invitado
	}
	return r
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

// esDisco manda una petición IN (tipo 0) u OUT (1) al disco de pruebas con
// un búfer por cada lens (khv_prueba_es); datos va y vuelve. Devuelve el byte
// de estado (-1 si no se pudo preparar) y la longitud del used.
func esDisco(tipo uint32, sector uint64, lens []uint32, datos []byte, malo int) (estado int, usado uint32) {
	var u C.uint32_t
	var pl *C.uint32_t
	if len(lens) > 0 {
		pl = (*C.uint32_t)(unsafe.Pointer(&lens[0]))
	}
	var pd *C.uint8_t
	if len(datos) > 0 {
		pd = (*C.uint8_t)(unsafe.Pointer(&datos[0]))
	}
	e := C.khv_prueba_es(C.uint32_t(tipo), C.uint64_t(sector), pl, C.int(len(lens)), pd, C.int(malo), &u)
	return int(e), uint32(u)
}

// tandaDisco llena la cola del disco de pruebas con 512 WRITE del disco
// entero y atiende una tanda (khv_prueba_tanda) con la VM pausándose o no y
// con el tope de bytes tope (0: el de siempre). Devuelve cuántas respondió y
// el estado de la última.
func tandaDisco(pausa bool, tope uint64) (hechas, estado int) {
	var e C.int
	h := C.khv_prueba_tanda(C.bool(pausa), C.uint64_t(tope), &e)
	return int(h), int(e)
}

// rtcRestaurado restaura el RTC con offset (khv_prueba_rtc) y devuelve lo que
// lee el invitado en DR y el offset con el que quedó.
func rtcRestaurado(offset int64) (dr uint32, guardado int64) {
	var g C.int64_t
	d := C.khv_prueba_rtc(C.int64_t(offset), &g)
	return uint32(d), int64(g)
}

// cfgDisco lee 4 bytes de los registros o la configuración (desde 0x100)
// del disco de pruebas.
func cfgDisco(off uint32) uint32 { return uint32(C.khv_prueba_cfg(C.uint32_t(off))) }

// modoDisco cambia si el disco de pruebas es de solo lectura y su modo de
// sincronización (C.KHV_SINC_*).
func modoDisco(ro bool, sinc int) { C.vblk_prueba_modo(0, C.bool(ro), C.int(sinc)) }

// Los modos de sincronización de khv.h, para las pruebas.
const (
	sincFull    = int(C.KHV_SINC_FULL)
	sincBarrier = int(C.KHV_SINC_BARRIER)
	sincNone    = int(C.KHV_SINC_NONE)
)

// bloquesDisco: los bloques de 512 B que ocupa el fichero del disco de
// pruebas (st_blocks), o -1.
func bloquesDisco() int64 { return int64(C.vblk_prueba_bloques(0)) }

// vsockTx manda como el invitado los bytes de enviado (4000 y cinco de
// 64 KiB) en seis paquetes por la cola de tx (khv_vsock_prueba_tx) y devuelve lo que leyó Go,
// si algo tuvo que esperar en pend, y el código de error (negativo) si no se
// pudo preparar.
func vsockTx(enviado []byte) (leido []byte, usoPend bool, ret int) {
	out := make([]byte, len(enviado)+1)
	var p C.int
	r := C.khv_vsock_prueba_tx((*C.uint8_t)(unsafe.Pointer(&out[0])), C.uint32_t(len(out)),
		(*C.uint8_t)(unsafe.Pointer(&enviado[0])), &p)
	if r < 0 {
		return nil, false, int(r)
	}
	return out[:r], p != 0, 0
}

// vsockFdEnPoll libera una conexión cuyo fd espera vs_hilo en poll
// (khv_vsock_prueba_fd_en_poll): 0 si no se cerró desde este hilo y Go vio
// el fin cuando lo cerró el hilo.
func vsockFdEnPoll() int { return int(C.khv_vsock_prueba_fd_en_poll()) }

// construirFDT llama a fdt_build con una VM de mentira (ncpus vCPU, 256 MiB,
// un virtio) y devuelve el DTB, o nil si no se pudo construir.
func construirFDT(ncpus int) []byte { return construirFDTOpts(ncpus, false) }

// construirFDTAnidada: lo mismo con 2 vCPU, con EL2 o sin él.
func construirFDTAnidada(anidada bool) []byte { return construirFDTOpts(2, anidada) }

func construirFDTOpts(ncpus int, anidada bool) []byte {
	buf := make([]byte, 1<<16)
	args := C.CString("console=ttyS0")
	defer C.free(unsafe.Pointer(args))
	p := C.struct_fdt_params{ram_size: 256 << 20, ncpus: C.int(ncpus), bootargs: args, dist_size: 0x10000,
		redist_size: 0x20000, uart_spi: 33, rtc_spi: 34, nvirtio: 1}
	p.virtio_spi[0] = 35
	if anidada {
		p.nested = 1
	}
	n := C.fdt_build(unsafe.Pointer(&buf[0]), C.size_t(len(buf)), &p)
	if n == 0 {
		return nil
	}
	return buf[:n]
}

// mpidrAff y mpidrIdx: khv_mpidr_aff y khv_mpidr_idx de khv.h.
func mpidrAff(i int) uint64          { return uint64(C.khv_mpidr_aff(C.int(i))) }
func mpidrIdx(aff uint64, n int) int { return int(C.khv_mpidr_idx(C.uint64_t(aff), C.int(n))) }

// maxVCPUsC es khv_max_vcpus (no crea ninguna VM).
func maxVCPUsC() int { return int(C.khv_max_vcpus()) }

// usedDelDispositivo: ver khv_prueba_used en prueba.c.
func usedDelDispositivo() int { return int(C.khv_prueba_used()) }

// pollRed y pollVsock: un poll que falla con EAGAIN no mata el hilo de la
// red ni el del vsock (khv_prueba_poll_red, khv_vsock_prueba_poll).
func pollRed() int   { return int(C.khv_prueba_poll_red()) }
func pollVsock() int { return int(C.khv_prueba_poll_vsock()) }

// irqNivelSinGIC: el nivel que deja vmmio_irq cuando el GIC no cambia la
// línea (khv_prueba_irq_nivel).
func irqNivelSinGIC() int { return int(C.khv_prueba_irq_nivel()) }

// vsockTandaRx: una vuelta de bombear del vsock con el socket del host lleno
// (khv_vsock_prueba_tanda_rx): cuánto había, cuánto entró y cuánto tuvo mu.
func vsockTandaRx(pausa bool) (lleno, movido uint64, d time.Duration, r int) {
	var l, m C.uint64_t
	var ns C.int64_t
	r = int(C.khv_vsock_prueba_tanda_rx(C.bool(pausa), &l, &m, &ns))
	return uint64(l), uint64(m), time.Duration(ns), r
}

// vsockMuerto: un poll que falla sin remedio termina el hilo del vsock; lo
// que tarda después un dial (khv_vsock_prueba_muerto).
func vsockMuerto() (int, time.Duration) {
	var ns C.int64_t
	r := int(C.khv_prueba_vsock_muerto(&ns))
	return r, time.Duration(ns)
}
