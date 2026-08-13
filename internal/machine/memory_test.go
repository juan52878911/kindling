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
	release, err := m.reserveMemory(grande, "")
	if err != nil {
		t.Fatalf("la primera reserva debería caber: %v", err)
	}

	// Con esa reserva viva, una segunda del mismo tamaño ya NO cabe, aunque el
	// MemAvailable crudo diga que sí: es justo lo que el TOCTOU no veía.
	if _, err := m.reserveMemory(grande, ""); err == nil {
		t.Error("la segunda reserva pasó: el TOCTOU sigue abierto")
	} else if !api.IsInsufficientMemory(err) {
		t.Errorf("el error no es de memoria insuficiente: %v", err)
	}

	// Liberada la primera, la segunda vuelve a caber.
	release()
	release2, err := m.reserveMemory(grande, "")
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

// Las copias de un mismo snapshot dorado comparten el mem.file, así que la
// segunda y siguientes solo reservan su fracción divergente. Es lo que convierte
// la densidad en real bajo arranques concurrentes.
func TestReservaCompartidaCobraFraccionALasCopias(t *testing.T) {
	m := newTestManager(t)
	avail := availableMiB()
	const want = 256
	frac := want / shareReserveDiv()
	if avail <= 0 || avail < want+frac+hostReserveMiB {
		t.Skip("sin margen de memoria para el caso (o macOS sin /proc/meminfo)")
	}

	// Primera instancia del snapshot: ancla el mem.file, paga entero.
	rel1, err := m.reserveMemory(want, "ctx")
	if err != nil {
		t.Fatalf("la primera debía caber: %v", err)
	}
	if p := pend(m); p != want {
		t.Fatalf("la primera debía reservar %d entero, reservó %d", want, p)
	}

	// Con una ya arrancando, la copia solo añade su fracción.
	rel2, err := m.reserveMemory(want, "ctx")
	if err != nil {
		t.Fatalf("la copia debía caber (paga fracción): %v", err)
	}
	if p := pend(m); p != want+frac {
		t.Fatalf("la copia debía añadir %d (want/%d); total esperado %d, hubo %d",
			frac, shareReserveDiv(), want+frac, p)
	}
	rel1()
	rel2()
	if p := pend(m); p != 0 {
		t.Fatalf("tras liberar todo, pendingMiB debía ser 0, es %d", p)
	}

	// Una instancia RUNNING del snapshot ancla igual, aunque no haya pendientes.
	m.mu.Lock()
	m.byID["x"] = &api.Machine{ID: "x", From: "svc", State: api.StateRunning}
	m.mu.Unlock()
	rel3, err := m.reserveMemory(want, "svc")
	if err != nil {
		t.Fatalf("con una running anclando, la copia debía caber: %v", err)
	}
	if p := pend(m); p != frac {
		t.Fatalf("con el mem.file anclado por una running, debía cobrar %d; reservó %d", frac, p)
	}
	rel3()

	// Un cold boot (sin shareKey) paga entero aunque el snapshot esté anclado.
	rel4, err := m.reserveMemory(want, "")
	if err != nil {
		t.Fatalf("cold boot debía caber: %v", err)
	}
	if p := pend(m); p != want {
		t.Fatalf("un cold boot no comparte: debía reservar %d entero, reservó %d", want, p)
	}
	rel4()
}

func pend(m *Manager) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.pendingMiB
}

// El suelo de MemFree atrapa el caso que MemAvailable no ve (muchos snapshots
// distintos vivos): con un suelo altísimo, checkHostMemory rechaza con 507 limpio
// aunque MemAvailable diga que cabe.
func TestSueloDeMemoriaLibre(t *testing.T) {
	if freeMiB() <= 0 {
		t.Skip("sin /proc/meminfo (macOS): el suelo se prueba en Linux")
	}
	t.Setenv("KLING_MIN_FREE_MIB", "99999999")
	err := checkHostMemory(16)
	if err == nil {
		t.Fatal("con suelo altísimo debía rechazar por MemFree")
	}
	if !api.IsInsufficientMemory(err) {
		t.Errorf("el rechazo no es 507: %v", err)
	}
	for _, q := range []string{"realmente libre", "suelo"} {
		if !strings.Contains(strings.ToLower(err.Error()), q) {
			t.Errorf("el mensaje del suelo no menciona %q: %v", q, err)
		}
	}
	// Suelo 0 lo desactiva: 16 MiB vuelven a pasar.
	t.Setenv("KLING_MIN_FREE_MIB", "0")
	if err := checkHostMemory(16); err != nil {
		t.Errorf("con el suelo desactivado, 16 MiB deberían caber: %v", err)
	}
}
