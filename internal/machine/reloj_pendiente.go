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

var (
	relojesMu  sync.Mutex
	relojesPen = map[string]chan struct{}{} // id -> se cierra al acabar
)

// relojPendiente apunta que la máquina id tiene un resync pendiente y
// devuelve la función que lo da por acabado (idempotente).
func (m *Manager) relojPendiente(id string) (fin func()) {
	ch := make(chan struct{})
	relojesMu.Lock()
	if viejo, ok := relojesPen[id]; ok {
		close(viejo) // un thaw nuevo sustituye al anterior
	}
	relojesPen[id] = ch
	relojesMu.Unlock()
	var una sync.Once
	return func() {
		una.Do(func() {
			relojesMu.Lock()
			defer relojesMu.Unlock()
			if relojesPen[id] == ch {
				delete(relojesPen, id)
				close(ch)
			}
		})
	}
}

// esperarRelojPendiente espera hasta relojEsperaMax a que acabe el resync
// pendiente de id, si lo hay. Devuelve true si sigue pendiente al volver (tope
// o ctx).
func (m *Manager) esperarRelojPendiente(ctx context.Context, id string) bool {
	relojesMu.Lock()
	ch := relojesPen[id]
	relojesMu.Unlock()
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
