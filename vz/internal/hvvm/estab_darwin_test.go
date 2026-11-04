//go:build darwin && arm64

package hvvm

import (
	"bytes"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Una consola cuyo lector no lee nunca no puede bloquear al que escribe en la
// UART (el hilo del vCPU): lo que no cabe se tira, se cuenta y, cuando el
// lector vuelve, lo primero que ve es el aviso de la pérdida.
func TestConsolaSinLectorNoBloquea(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()
	uartIniciar(int(pw.Fd()))
	defer uartIniciar(-1)

	linea := []byte(strings.Repeat("y", 63) + "\n")
	hecho := make(chan struct{})
	go func() {
		defer close(hecho)
		// 4 MiB: muchas veces lo que cabe en la tubería (64 KiB).
		for i := 0; i < 1<<16; i++ {
			uartEscribir(linea)
		}
		uartVaciar()
	}()
	select {
	case <-hecho:
	case <-time.After(20 * time.Second):
		t.Fatal("la UART se bloqueó con el lector atascado")
	}
	perdidos := ConsoleDropped()
	if perdidos == 0 {
		t.Fatal("con el lector atascado no se tiró nada: ¿escribió todo en la tubería?")
	}
	t.Logf("bytes tirados: %d", perdidos)

	// El lector vuelve: se vacía la tubería y la siguiente línea llega tras
	// el aviso.
	// Fd() vuelve a dejar el fd bloqueante: se toma una vez.
	rfd := int(pr.Fd())
	if err := syscall.SetNonblock(rfd, true); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1<<20)
	for {
		n, err := syscall.Read(rfd, buf)
		if n <= 0 || err != nil {
			break
		}
	}
	uartEscribir([]byte("vuelve\n"))
	n, err := syscall.Read(rfd, buf)
	if err != nil {
		t.Fatal(err)
	}
	got := buf[:n]
	if !bytes.Contains(got, []byte("console reader stalled")) || !bytes.HasSuffix(got, []byte("vuelve\n")) {
		t.Fatalf("tras volver el lector: %q", got)
	}
}

// vnet_parar despierta al hilo de recepción, que espera en poll, y lo espera:
// después nadie lee del socket, que Go ya puede cerrar.
func TestRedPararSueltaElFd(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])
	netIniciar(fds[0])
	time.Sleep(20 * time.Millisecond) // que el hilo llegue a su poll
	hecho := make(chan struct{})
	go func() {
		netParar()
		close(hecho)
	}()
	select {
	case <-hecho:
	case <-time.After(5 * time.Second):
		t.Fatal("vnet_parar no volvió: el hilo de recepción sigue esperando")
	}
	netParar() // idempotente

	// Una trama tras parar se queda en el socket: no hay hilo que la lea.
	if _, err := syscall.Write(fds[1], []byte("trama")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	buf := make([]byte, 64)
	n, _, err := syscall.Recvfrom(fds[0], buf, syscall.MSG_DONTWAIT)
	if err != nil || string(buf[:n]) != "trama" {
		t.Fatalf("la trama tras vnet_parar: %q, %v (¿la leyó el hilo?)", buf[:max(n, 0)], err)
	}
}

// Un dial que vence sin que el invitado reponga búferes de rx suelta su
// slot: antes quedaba con el RST pendiente para siempre y 64 así agotaban
// los slots.
func TestVsockDialVencidoSueltaSlot(t *testing.T) {
	for _, enviada := range []bool{false, true} {
		if got := vsockPlazo(enviada); got != 1 {
			t.Fatalf("petición enviada=%v: %d, quiero 1 (0 = slot retenido)", enviada, got)
		}
	}
	// Más dials vencidos que slots: si se retuvieran, el último daría EMFILE.
	for i := 0; i < 70; i++ {
		if got := vsockPlazo(i%2 == 1); got != 1 {
			t.Fatalf("dial vencido %d: %d, quiero 1", i, got)
		}
	}
}
