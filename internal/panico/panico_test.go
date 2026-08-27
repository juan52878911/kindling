package panico

import (
	"log"
	"strings"
	"testing"
)

// Sin Contener, este test NO falla: se lleva por delante el proceso de tests
// entero. Esa es exactamente la demostracion de lo que pasaria en el daemon.
func TestContenerDejaSeguirAlBucle(t *testing.T) {
	vueltas := 0
	for i := 0; i < 3; i++ {
		Contener("prueba", func() {
			vueltas++
			if i == 0 {
				panic("la primera vuelta explota")
			}
		})
	}
	if vueltas != 3 {
		t.Errorf("dieron %d vueltas de 3: el panico se llevo el bucle", vueltas)
	}
}

func TestContenerDejaConstanciaConTraza(t *testing.T) {
	var buf strings.Builder
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	Contener("el-reconciliador", func() { panic("nil map") })

	salida := buf.String()
	for _, quiero := range []string{"el-reconciliador", "nil map", "panico.Contener"} {
		if !strings.Contains(salida, quiero) {
			t.Errorf("el log no menciona %q; sin eso el panico es invisible:\n%s", quiero, salida)
		}
	}
}

func TestContenerNoEstorbaCuandoNoHayPanico(t *testing.T) {
	llamada := false
	Contener("normal", func() { llamada = true })
	if !llamada {
		t.Error("Contener no ejecuto fn")
	}
}
