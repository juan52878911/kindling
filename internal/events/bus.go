// Package events implementa el bus interno del daemon.
//
// Publish nunca bloquea: si un suscriptor no consume a tiempo, pierde eventos en
// lugar de frenar el ciclo de vida de las microVMs. Un observador lento no debe
// poder atascar un arranque.
package events

import (
	"sync"

	"github.com/juan52878911/kindling/internal/api"
)

const subBuffer = 64

// Bus reparte eventos a los suscriptores activos.
type Bus struct {
	mu   sync.RWMutex
	subs map[int]chan api.Event
	next int
}

func New() *Bus {
	return &Bus{subs: make(map[int]chan api.Event)}
}

// Subscribe devuelve un canal de eventos y la función para darse de baja.
func (b *Bus) Subscribe() (<-chan api.Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	id := b.next
	b.next++
	ch := make(chan api.Event, subBuffer)
	b.subs[id] = ch

	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
	}
}

// Publish entrega el evento a quien esté escuchando. No bloquea.
func (b *Bus) Publish(ev api.Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default: // suscriptor lento: se descarta y seguimos
		}
	}
}
