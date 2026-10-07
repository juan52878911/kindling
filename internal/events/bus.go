// Package events implementa el bus interno del daemon.
//
// Publish nunca bloquea: si un suscriptor no consume a tiempo, pierde eventos en
// lugar de frenar el ciclo de vida de las microVMs. Un observador lento no debe
// poder atascar un arranque. Pero no los pierde a ciegas: el bus cuenta los
// descartes (Descartados, para /metrics) y, en cuanto el suscriptor vuelve a
// tener sitio, le entrega un api.EvDropped con cuántos se perdió antes del
// siguiente evento.
package events

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

const subBuffer = 64

// Bus reparte eventos a los suscriptores activos.
type Bus struct {
	mu   sync.RWMutex
	subs map[int]*suscriptor
	next int

	descartados atomic.Int64
}

// suscriptor es un canal y lo que se ha perdido desde su último aviso. mu pone
// en fila a los Publish concurrentes (todos bajo el RLock del bus) sobre ESTE
// suscriptor: sin él, dos podrían avisar de los mismos descartes.
type suscriptor struct {
	mu       sync.Mutex
	ch       chan api.Event
	perdidos int64
}

func New() *Bus {
	return &Bus{subs: make(map[int]*suscriptor)}
}

// Subscribe devuelve un canal de eventos y la función para darse de baja.
func (b *Bus) Subscribe() (<-chan api.Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	id := b.next
	b.next++
	s := &suscriptor{ch: make(chan api.Event, subBuffer)}
	b.subs[id] = s

	return s.ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c.ch)
		}
	}
}

// Publish entrega el evento a quien esté escuchando. No bloquea.
func (b *Bus) Publish(ev api.Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, s := range b.subs {
		if !s.entregar(ev) {
			b.descartados.Add(1)
		}
	}
}

// Descartados es cuántos eventos se han perdido en total, sumando todos los
// suscriptores, desde que arrancó el daemon.
func (b *Bus) Descartados() int64 { return b.descartados.Load() }

// entregar intenta dejar ev en el canal; dice si cupo. Si antes se perdió
// algo, primero va el aviso (que ocupa un hueco): el suscriptor lo lee en su
// sitio, justo antes de lo que viene después del agujero.
func (s *suscriptor) entregar(ev api.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perdidos > 0 {
		aviso := api.Event{Time: time.Now(), Type: api.EvDropped, Dropped: s.perdidos,
			Message: fmt.Sprintf("%d event(s) dropped: this subscriber did not read them in time", s.perdidos)}
		select {
		case s.ch <- aviso:
			s.perdidos = 0
		default:
			s.perdidos++
			return false
		}
	}
	select {
	case s.ch <- ev:
		return true
	default: // suscriptor lento: se descarta y seguimos
		s.perdidos++
		return false
	}
}
