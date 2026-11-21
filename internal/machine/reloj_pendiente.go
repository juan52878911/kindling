package machine

import (
	"context"
	"log"
	"sync"
	"time"
)

// Un thaw cuyo resync falla entrega la máquina igual y reintenta en segundo
// plano (resyncThawReintentos). Un proceso que arrancara en ese hueco (un exec
// con TLS) vería la hora de cuando se congeló. Aquí queda apuntado, por
// máquina, que hay un resync así pendiente: ExecTarget (exec, sesiones,
// ficheros) espera a que acabe, con un tope.

// relojEsperaMax es lo máximo que un exec espera a un resync pendiente; los
// tests lo acortan. Pasado el tope sigue, con aviso en el log.
var relojEsperaMax = 2 * time.Second

// relojesPendientes es el campo Manager.relojes: por máquina, un canal que se
// cierra al acabar su resync. Su valor cero sirve.
type relojesPendientes struct {
	mu  sync.Mutex
	pen map[string]chan struct{}
}

// relojPendiente apunta que la máquina id tiene un resync pendiente y
// devuelve la función que lo da por acabado (idempotente).
func (m *Manager) relojPendiente(id string) (fin func()) {
	ch := make(chan struct{})
	r := &m.relojes
	r.mu.Lock()
	if viejo, ok := r.pen[id]; ok {
		close(viejo) // un thaw nuevo sustituye al anterior
	}
	if r.pen == nil {
		r.pen = map[string]chan struct{}{}
	}
	r.pen[id] = ch
	r.mu.Unlock()
	var una sync.Once
	return func() {
		una.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.pen[id] == ch {
				delete(r.pen, id)
				close(ch)
			}
		})
	}
}

// esperarRelojPendiente espera hasta relojEsperaMax a que acabe el resync
// pendiente de id, si lo hay. Devuelve true si sigue pendiente al volver (tope
// o ctx).
func (m *Manager) esperarRelojPendiente(ctx context.Context, id string) bool {
	m.relojes.mu.Lock()
	ch := m.relojes.pen[id]
	m.relojes.mu.Unlock()
	if ch == nil {
		return false
	}
	t := time.NewTimer(relojEsperaMax)
	defer t.Stop()
	select {
	case <-ch:
		return false
	case <-ctx.Done():
	case <-t.C:
		log.Printf("%s: continuing without waiting for the pending clock resync (waited %s): the guest may still have the clock of the freeze", shortID(id), relojEsperaMax)
	}
	return true
}
