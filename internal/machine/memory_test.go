package machine

import (
	"github.com/juan52878911/kindling/internal/api"
	"strings"
	"testing"
)

// No arrancar lo que no cabe. Una microVM que desborda el anfitrión no falla al
// arrancar: arranca, y después el OOM killer del host mata procesos al azar
// —incluidas otras microVMs y el propio daemon—, con el motivo en un journal
// que nadie mira.
func TestNoSeArrancaLoQueNoCabe(t *testing.T) {
	avail := availableMiB()
	if avail <= 0 {
		t.Skip("no puedo leer la memoria de este host (macOS): la comprobación se salta sola")
	}

	// Lo que cabe de sobra, pasa.
	if err := checkHostMemory(16); err != nil {
		t.Errorf("rechazó 16 MiB habiendo %d disponibles: %v", avail, err)
	}
	// Lo que no cabe ni de lejos, no.
	err := checkHostMemory(avail + hostReserveMiB + 1024)
	if err == nil {
		t.Fatal("aceptó una microVM más grande que el anfitrión")
	}
	// Y el error tiene que decir las dos cifras y qué hacer, o no sirve de nada.
	for _, quiero := range []string{"no cabe", "reserva", "kling ps -a"} {
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(quiero)) {
			t.Errorf("el error no menciona %q: %v", quiero, err)
		}
	}
}

// Sin poder leer la memoria NO se bloquea nada: convertir una comprobación de
// cordura en una dependencia de arranque sería peor que el problema que evita.
func TestSinDatosDeMemoriaNoSeBloquea(t *testing.T) {
	if err := checkHostMemory(0); err != nil {
		t.Errorf("bloqueó una petición sin memoria declarada: %v", err)
	}
}

// reserveMemory cierra el TOCTOU: dos arranques concurrentes no pueden ver los
// dos la misma memoria libre y pasar.
//
// Sin la reserva, entre leer MemAvailable y que el invitado ocupe su RAM pasan
// segundos, y en ese hueco un segundo arranque veía la misma memoria y se sumaba
// al desbordamiento — el OOM del anfitrión que el check existe para impedir.
func TestLaReservaDeMemoriaNoSeCuentaDosVeces(t *testing.T) {
	m := newTestManager(t)
	avail := availableMiB()
	if avail <= 0 {
		t.Skip("sin /proc/meminfo (macOS): la reserva se prueba en Linux")
	}

	// Reservar casi toda la memoria disponible: la primera pasa.
	grande := avail - hostReserveMiB - 64
	if grande <= 0 {
		t.Skip("el host de test no tiene memoria de sobra para el caso")
	}
	release, err := m.reserveMemory(grande)
	if err != nil {
		t.Fatalf("la primera reserva debería caber: %v", err)
	}

	// Con esa reserva viva, una segunda del mismo tamaño ya NO cabe, aunque el
	// MemAvailable crudo diga que sí: es justo lo que el TOCTOU no veía.
	if _, err := m.reserveMemory(grande); err == nil {
		t.Error("la segunda reserva pasó: el TOCTOU sigue abierto")
	} else if !api.IsInsufficientMemory(err) {
		t.Errorf("el error no es de memoria insuficiente: %v", err)
	}

	// Liberada la primera, la segunda vuelve a caber.
	release()
	release2, err := m.reserveMemory(grande)
	if err != nil {
		t.Errorf("tras liberar debería caber otra vez: %v", err)
	} else {
		release2()
	}

	// Y liberar dos veces no deja el contador en negativo (bloquearía a todos).
	release()
	m.mu.RLock()
	pend := m.pendingMiB
	m.mu.RUnlock()
	if pend < 0 {
		t.Errorf("pendingMiB quedó negativo: %d", pend)
	}
}
