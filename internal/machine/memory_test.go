package machine

import (
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
