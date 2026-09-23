package machine

import (
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// El disco lleno se rechaza con un código que el planificador NO interpreta como
// falta de memoria: si lo hiciera, congelaría instancias, y congelar escribe en
// disco lo que falta.
func TestDiscoLlenoNoSeConfundeConMemoria(t *testing.T) {
	m := newTestManager(t)
	t.Setenv("KLING_MIN_FREE_DISK_MIB", "999999999") // más de lo que hay en cualquier sitio
	err := m.checkDisk()
	if err == nil {
		t.Fatal("con un mínimo imposible tenía que rechazar")
	}
	if !api.IsDiskFull(err) {
		t.Fatalf("no se reconoce como disco lleno: %v", err)
	}
	if api.IsInsufficientMemory(err) {
		t.Fatal("se confunde con falta de memoria: el planificador congelaría para hacer sitio")
	}

	t.Setenv("KLING_MIN_FREE_DISK_MIB", "0")
	if err := m.checkDisk(); err != nil {
		t.Fatalf("con la comprobación apagada: %v", err)
	}
}

// La presión de memoria se puede apagar, y sin PSI (macOS, kernels sin él) no
// bloquea nada: una comprobación de cordura no puede volverse un requisito.
func TestPresionDeMemoria(t *testing.T) {
	t.Setenv("KLING_MAX_MEM_PRESSURE", "0")
	if err := checkPressure(); err != nil {
		t.Fatalf("apagada: %v", err)
	}
	t.Setenv("KLING_MAX_MEM_PRESSURE", "")
	if p := memPressure(); p < 0 {
		if err := checkPressure(); err != nil {
			t.Fatalf("sin PSI no debe bloquear: %v", err)
		}
	}
	// Con PSI y un tope negativo imposible de cumplir... no hay forma de forzar
	// presión en un test; lo que se comprueba es el camino de lectura.
	if p := memPressure(); p > 100 {
		t.Fatalf("PSI fuera de rango: %v", p)
	}
}
