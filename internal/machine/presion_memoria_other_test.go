//go:build !darwin

package machine

import (
	"errors"
	"testing"
)

// Linux: la PSI de memoria se traduce a los niveles de macOS, y sin PSI no hay
// señal (y tampoco aviso en el log).
func TestNivelPresionPSI(t *testing.T) {
	for _, c := range []struct {
		avg10 float64
		nivel int
	}{{0, NivelPresionNormal}, {9.9, NivelPresionNormal}, {10, NivelPresionAviso}, {39.9, NivelPresionAviso}, {40, NivelPresionCritico}, {95, NivelPresionCritico}} {
		if n, err := nivelPresionPSI(c.avg10); err != nil || n != c.nivel {
			t.Fatalf("avg10 %v: %d %v, quería %d", c.avg10, n, err, c.nivel)
		}
	}
	if _, err := nivelPresionPSI(-1); !errors.Is(err, errSinPresion) {
		t.Fatalf("sin PSI: %v", err)
	}
}
