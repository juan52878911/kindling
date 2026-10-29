package daemon

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/juan52878911/kindling/internal/machine"
)

// Lo que falla en el host (un EIO, un disco que no se deja escribir) es 500
// en todas las familias de rutas; antes snapshots, grafos, store, volúmenes,
// populate y cow/store/grow lo contestaban 400, como si lo hubiera pedido mal
// el cliente, y renew 502. Lo de la petición sigue siendo 400, y el agente de
// una copia que no contesta es 504 como en POST /sandboxes.
func TestCodigosDelHost(t *testing.T) {
	eio := errors.New("input/output error")
	for _, c := range []struct {
		nombre string
		f      func(error) int
		err    error
		quiero int
	}{
		{"snapshot host", snapshotStatus, eio, 500},
		{"snapshot petición", snapshotStatus, fmt.Errorf("x: %w", machine.ErrRunRequest), 400},
		{"snapshot nombre", snapshotStatus, machine.ErrInvalidName, 400},
		{"snapshot no existe", snapshotStatus, machine.ErrNoSnapshot, 404},
		{"grafo host", graphStatus, eio, 500},
		{"grafo no existe", graphStatus, machine.ErrNoGraph, 404},
		{"grafo nodo con imagen que no existe", graphStatus, fmt.Errorf("node a: %w", machine.ErrNoImage), 404},
		{"store host", storeStatus, eio, 500},
		{"store petición", storeStatus, storeMal("invalid store key %q", "K"), 400},
		{"volumen host", volumeStatus, eio, 500},
		{"volumen en uso", volumeStatus, machine.ErrVolumenEnUso, 409},
		{"volumen ya existe", volumeStatus, fmt.Errorf("v: %w", os.ErrExist), 409},
		{"volumen no existe", volumeStatus, machine.ErrNoVolume, 404},
		{"volumen nombre", volumeStatus, machine.ErrInvalidName, 400},
		{"renew host", renewStatus, eio, 500},
		{"renew parada", renewStatus, machine.ErrNotRunning, 409},
		{"renew no existe", renewStatus, machine.ErrNoMachine, 404},
		{"fork agente callado", forkStatus, fmt.Errorf("fork 1 of 1: %w", &errAgenteCallado{eio}), 504},
		{"fork host", forkStatus, eio, 500},
		{"fichero de imagen sin imagen", imageFileStatus, machine.ErrNoImage, 404},
		{"fichero de imagen sin debugfs", imageFileStatus, machine.ErrNoDebugfs, 500},
		{"fichero de imagen ruta", imageFileStatus, errors.New("path must be absolute"), 400},
	} {
		if got := c.f(c.err); got != c.quiero {
			t.Errorf("%s: %d, quería %d", c.nombre, got, c.quiero)
		}
	}
}
