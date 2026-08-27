package api

import (
	"errors"
	"fmt"
	"testing"
)

// El texto crudo es el que devolvio Firecracker en el laboratorio despues de
// reiniciar el anfitrion. Si algun dia cambia, este test lo dira.
const errTSCCrudo = "firecracker PUT /snapshot/load: 400 : Load snapshot error: " +
	"Failed to restore from snapshot: Failed to build microVM from snapshot: " +
	"Could not set TSC scaling within the snapshot: Invalid argument (os error 22)"

func TestEsFalloTSCDistingueLaCausaCurableDeLasDemas(t *testing.T) {
	casos := []struct {
		nombre string
		err    error
		quiero bool
	}{
		{"el error crudo de Firecracker", errors.New(errTSCCrudo), true},
		{"envuelto por el camino", fmt.Errorf("didn't start (%w)", errors.New(errTSCCrudo)), true},
		{"sin error", nil, false},
		{"el servidor MCP no contesta", errors.New("didn't respond to tools/list (timeout)"), false},
		{"falta un secreto", errors.New("missing required env GITHUB_TOKEN"), false},
		{"disco lleno", errors.New("no space left on device"), false},
	}
	for _, c := range casos {
		if got := EsFalloTSC(c.err); got != c.quiero {
			t.Errorf("%s: EsFalloTSC = %v, esperaba %v", c.nombre, got, c.quiero)
		}
	}
}
