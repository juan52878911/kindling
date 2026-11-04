//go:build darwin && arm64

package hvvm

/*
#include "khv.h"

// De devices.c, net.c y vsock.c (khv.h es de vm.c).
uint64_t uart_perdidos(void);
void vnet_parar(void);
int khv_vsock_prueba_plazo(int enviada);
*/
import "C"

// Solo para las pruebas (estab_darwin_test.go): llaman a los dispositivos sin
// VM y no pueden convivir con una de verdad en el mismo proceso.

// uartIniciar apunta la consola de la UART a fd (-1: a ninguna).
func uartIniciar(fd int) { C.uart_init(33, C.int(fd)) }

// uartEscribir manda b al registro THR, byte a byte, como el driver 8250.
func uartEscribir(b []byte) {
	for _, c := range b {
		v := C.uint64_t(c)
		C.uart_mmio(0, true, &v)
	}
}

// uartVaciar saca la línea a medias que quede en el búfer.
func uartVaciar() { C.uart_flush() }

// netIniciar arranca el hilo de recepción de la red sobre fd.
func netIniciar(fd int) {
	mac := [6]C.uint8_t{0x06, 0, 0xac, 0x10, 0, 2}
	C.vnet_init(C.int(fd), &mac[0], 0, 40)
}

// netParar es vnet_parar.
func netParar() { C.vnet_parar() }

// vsockPlazo: un dial que vence sin búferes de rx del invitado
// (khv_vsock_prueba_plazo).
func vsockPlazo(enviada bool) int {
	e := C.int(0)
	if enviada {
		e = 1
	}
	return int(C.khv_vsock_prueba_plazo(e))
}
