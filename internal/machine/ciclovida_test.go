package machine

import (
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/api"
	"github.com/juan52878911/kindling/internal/events"
)

// Stop no tomaba el cerrojo de ciclo de vida, que SI tienen Freeze, Thaw,
// Squeeze, PutMMDS y Remove.
//
// La consecuencia no es teorica: Stop mata el VMM y DESMONTA el namespace de
// red. Si corre a la vez que un Thaw, le quita la red por debajo — la maquina
// queda "running" y sin conectividad, y lo unico que se ve por arriba son
// timeouts del gateway que no apuntan a ninguna causa.
func TestStopEsperaAlCerrojoDeCicloDeVida(t *testing.T) {
	m := newTestManager(t)
	// newTestManager no monta el bus de eventos, y Stop/Remove publican al
	// terminar. En produccion NewManager siempre lo pone.
	m.bus = events.New()
	id := newID()
	m.byID[id] = &api.Machine{ID: id, Name: "la-maquina", State: api.StateRunning}

	soltar := m.lock(id) // otra operacion la tiene tomada
	hecho := make(chan struct{})
	go func() {
		_, _ = m.Stop(id)
		close(hecho)
	}()

	select {
	case <-hecho:
		t.Fatal("Stop no esperó al cerrojo: puede desmontar la red bajo un Thaw en curso")
	case <-time.After(200 * time.Millisecond):
		// Correcto: sigue esperando.
	}

	soltar()
	select {
	case <-hecho:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop no terminó tras soltar el cerrojo")
	}
	if m.byID[id].State != api.StateStopped {
		t.Errorf("estado = %s, esperaba stopped", m.byID[id].State)
	}
}

// Remove borraba su entrada del mapa de cerrojos ANTES del RemoveAll, que borra
// un mem.file de cientos de MB. Cualquier m.lock(id) entrante hacia entonces un
// LoadOrStore, creaba un mutex NUEVO y entraba en la seccion critica mientras
// Remove seguia borrando ficheros.
func TestRemoveNoSueltaSuCerrojoAntesDeTerminar(t *testing.T) {
	m := newTestManager(t)
	// newTestManager no monta el bus de eventos, y Stop/Remove publican al
	// terminar. En produccion NewManager siempre lo pone.
	m.bus = events.New()
	id := newID()
	m.byID[id] = &api.Machine{ID: id, Name: "la-maquina", State: api.StateStopped}

	// Se toma primero desde fuera: Remove tiene que esperar.
	soltar := m.lock(id)
	hecho := make(chan struct{})
	go func() {
		_ = m.Remove(id)
		close(hecho)
	}()
	select {
	case <-hecho:
		t.Fatal("Remove no esperó al cerrojo")
	case <-time.After(200 * time.Millisecond):
	}
	soltar()
	select {
	case <-hecho:
	case <-time.After(5 * time.Second):
		t.Fatal("Remove no terminó")
	}

	if _, sigue := m.byID[id]; sigue {
		t.Error("la máquina sigue en byID tras Remove")
	}
	// Y el cerrojo se retira, o el mapa crece sin fin con cada maquina borrada.
	if _, sigue := m.lifecycle.Load(id); sigue {
		t.Error("quedó la entrada del cerrojo: el mapa crecería con cada máquina retirada")
	}
}

// Stop sobre algo que ya no existe no es un error —el resultado que se pedia ya
// se cumple— y sobre todo NO puede ser un deref nil: en el daemon eso no se
// lleva la operacion, se lleva el proceso y con el todas las microVM.
func TestStopDeAlgoQueDesaparecioNoTumbaNada(t *testing.T) {
	m := newTestManager(t)
	// newTestManager no monta el bus de eventos, y Stop/Remove publican al
	// terminar. En produccion NewManager siempre lo pone.
	m.bus = events.New()
	id := newID()
	m.byID[id] = &api.Machine{ID: id, Name: "fantasma", State: api.StateRunning}

	// Se borra del mapa dejando la referencia que Stop ya obtuvo con Get.
	mc, ok := m.Get(id)
	if !ok {
		t.Fatal("Get no la encontró")
	}
	delete(m.byID, id)

	got, err := m.Stop(mc.ID)
	if err == nil {
		if got == nil || got.State != api.StateStopped {
			t.Errorf("resultado inesperado: %+v", got)
		}
		return
	}
	// Un error tambien vale —ya no existe—; lo que no vale es un panico.
}
