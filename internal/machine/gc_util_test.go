package machine

import "testing"

// Medido en fc-test: containerd ocupaba 7,8 GB de un disco de 20 y kindling 6,1.
// El disco no bajaba del 91%, asi que el GC expulsaba TODA instancia dormida cada
// diez segundos —13 en un dia, cada una 1-11 s despues de congelarse— sin ganar
// un byte. El resultado era que cada llamada pagaba un arranque en frio de 30-40 s
// y nada correlacionaba las dos cosas.
func TestEvictarSirveSoloSiBajaOAlMenosProgresa(t *testing.T) {
	const alta = 90

	casos := []struct {
		nombre         string
		antes, despues int
		sirve          bool
	}{
		{"bajo del umbral", 95, 80, true},
		{"justo por debajo", 95, 89, true},
		{"bajo algo pero sigue alto", 95, 92, true},
		{"no bajo nada: el disco no es nuestro", 91, 91, false},
		{"subio mientras expulsabamos", 91, 92, false},
		{"el caso del laboratorio", 91, 91, false},
	}
	for _, c := range casos {
		if got := evictarSirve(c.antes, c.despues, alta); got != c.sirve {
			t.Errorf("%s (%d%%→%d%%): sirve=%v, esperaba %v", c.nombre, c.antes, c.despues, got, c.sirve)
		}
	}
}
