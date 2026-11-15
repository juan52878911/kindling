package machine

// La parada del daemon con una preparación de snapshot por imagen en curso,
// el machine_id de los nodos de un grafo (lo pone el daemon, nunca la
// petición) y el veredicto de integridad de un snapshot borrado.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// Close espera a la preparación en curso y le deja borrar su máquina: antes
// la cancelaba sin esperarla, y su Remove (en un defer) llegaba con el cierre
// ya empezado y se negaba, o competía con la salida del proceso. La máquina
// de preparación (y su VMM) se quedaba sin dueño.
func TestCloseEsperaALaPreparacionDeImagen(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	m.priv = &Privileges{}
	prep := m.addForTest(prefijoPrepImagen + "0123abcd")
	empezo, acabo := make(chan struct{}), make(chan error, 1)
	m.img.probar = func(ctx context.Context, _ api.RunRequest, _ claveImagen) error {
		close(empezo)
		<-ctx.Done() // como esperarAgentePrep o commit: hasta que la cancelan
		// Lo que hace el defer de prepararImagen.
		err := m.Remove(prep.ID)
		acabo <- err
		return ctx.Err()
	}
	m.quizaPrepararImagen(api.RunRequest{Image: "min"}, claveImagen{Image: "min", VCPUs: 1, MemMiB: 256})
	select {
	case <-empezo:
	case <-time.After(5 * time.Second):
		t.Fatal("la preparación no empezó")
	}
	m.Close()
	select {
	case err := <-acabo:
		if err != nil {
			t.Fatalf("la preparación no pudo borrar su máquina durante el cierre: %v", err)
		}
	default:
		t.Fatal("Close volvió sin esperar a la preparación en curso")
	}
	m.EsperarBorrados(5 * time.Second)
	if _, ok := m.Get(prep.ID); ok {
		t.Fatal("la máquina de preparación sobrevivió al cierre")
	}
	// Y después del cierre no empieza otra.
	m.img.probar = func(context.Context, api.RunRequest, claveImagen) error {
		t.Error("una preparación empezó tras Close")
		return nil
	}
	m.quizaPrepararImagen(api.RunRequest{Image: "min"}, claveImagen{Image: "min", VCPUs: 1, MemMiB: 512})
	time.Sleep(50 * time.Millisecond)
}

// El machine_id de un nodo lo pone el daemon. Uno que llegue en la petición
// no se respeta: si up fallaba a medias, el desmontaje borraba la máquina que
// nombraba, aunque fuera de otro.
func TestGrafoUpIgnoraElMachineIDDeLaPeticion(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	ajena := e.m.addForTest("ajena00000000000")
	e.fallarArranque = func(nombre string) error {
		if nombre == "tienda-web" {
			return errors.New("imagen rota")
		}
		return nil
	}
	g := grafoTienda(true)
	db := g.Nodes["db"] // lazy: up no le crea máquina
	db.MachineID, db.State = ajena.ID, string(api.StateRunning)
	g.Nodes["db"] = db
	if _, err := e.m.GraphUp(context.Background(), g, nil); err == nil {
		t.Fatal("esperaba el fallo de web")
	}
	e.m.EsperarBorrados(5 * time.Second)
	if _, ok := e.m.Get(ajena.ID); !ok {
		t.Fatal("el desmontaje de un up fallido borró la máquina que nombraba el machine_id de la petición")
	}

	// Y uno que sube bien no se queda con ese id en su nodo lazy.
	e.fallarArranque = nil
	out, err := e.m.GraphUp(context.Background(), g, nil)
	if err != nil {
		t.Fatal(err)
	}
	if id := out.Nodes["db"].MachineID; id != "" {
		t.Fatalf("el nodo lazy quedó con machine_id %q, el de la petición", id)
	}
}

// Al borrar un snapshot se olvida su veredicto de integridad: si no, el mapa
// crece con cada nombre que existió.
func TestRemoveSnapshotOlvidaSuIntegridad(t *testing.T) {
	m := newTestManager(t)
	escribirSnapshot(t, m, "dorado", api.Snapshot{})
	m.anotarHuella("dorado", huellaSnapshot{})
	if err := m.removeSnapshot("dorado", false); err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	_, queda := m.integridad["dorado"]
	m.mu.RUnlock()
	if queda {
		t.Fatal("el veredicto de integridad del snapshot borrado sigue en memoria")
	}
}
