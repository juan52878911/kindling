package machine

// RETROCESO DE LA PUERTA.
//
// Si los VMM mueren solos (o una copia restaurada no llega a correr) con la
// puerta alta, lo más probable es una tormenta de KVM en anidado. En vez de
// seguir empujando, cada aviso baja el tope a la mitad (mínimo 2) y se
// recupera de uno en uno cada plazoRetroceso sin nuevos avisos.

import (
	"log"
	"sync"
	"time"
)

// plazoRetroceso es lo que dura cada escalón del retroceso.
const plazoRetroceso = 60 * time.Second

// minPuertaRetroceso es el suelo al que el retroceso baja el tope.
const minPuertaRetroceso = 2

// retrocesoPuerta calcula el tope efectivo de la puerta. Su reloj es
// inyectable para los tests.
type retrocesoPuerta struct {
	mu     sync.Mutex
	base   int
	bajo   int // tope justo tras el último retroceso
	desde  time.Time
	cuenta int
	reloj  func() time.Time
	activo bool
	// retenidos son los huecos de launchGate guardados para bajar el tope.
	retenidos int
}

func (r *retrocesoPuerta) ahora() time.Time {
	if r.reloj != nil {
		return r.reloj()
	}
	return time.Now()
}

// topeLocked es el tope vigente: lo bajado más un escalón por plazo cumplido.
func (r *retrocesoPuerta) topeLocked(ahora time.Time) int {
	if !r.activo {
		return r.base
	}
	return min(r.base, r.bajo+int(ahora.Sub(r.desde)/plazoRetroceso))
}

// tope es el tope vigente ahora.
func (r *retrocesoPuerta) tope() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.topeLocked(r.ahora())
}

// retroceder baja el tope a la mitad (sin pasar de minPuertaRetroceso ni
// subirlo) y reinicia el plazo. Devuelve el tope anterior, el nuevo y si
// bajó de verdad.
func (r *retrocesoPuerta) retroceder() (antes, ahora int, bajo bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.ahora()
	antes = r.topeLocked(t)
	ahora = min(antes, max(minPuertaRetroceso, antes/2))
	r.cuenta++
	r.activo, r.bajo, r.desde = true, ahora, t
	return antes, ahora, ahora < antes
}

// retrocesos es cuántos avisos ha recibido la puerta.
func (r *retrocesoPuerta) retrocesos() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cuenta
}

// retrocesos guarda el estado de retroceso de cada Manager con puerta. Va
// aparte para no tocar la estructura de Manager.
var retrocesosPuerta sync.Map // *Manager -> *retrocesoPuerta

// retrocesoDe devuelve el estado de m, creándolo con la capacidad de su puerta.
func (m *Manager) retrocesoDe() *retrocesoPuerta {
	if m.launchGate == nil {
		return nil
	}
	if r, ok := retrocesosPuerta.Load(m); ok {
		return r.(*retrocesoPuerta)
	}
	r, _ := retrocesosPuerta.LoadOrStore(m, &retrocesoPuerta{base: cap(m.launchGate)})
	return r.(*retrocesoPuerta)
}

// puertaRetroceder baja el tope de la puerta de arranque por motivo. Se
// registra una vez por escalón real.
func (m *Manager) puertaRetroceder(motivo string) {
	r := m.retrocesoDe()
	if r == nil {
		return
	}
	antes, ahora, bajo := r.retroceder()
	if bajo {
		log.Printf("launch gate: backing off from %d to %d simultaneous boots (%s); recovers one step per %s (backoffs so far: %d)",
			antes, ahora, motivo, plazoRetroceso, r.retrocesos())
	}
	m.ajustarPuerta()
}

// ajustarPuerta hace que la capacidad útil de launchGate sea el tope vigente:
// retiene huecos de más (sin esperar: los que estén en uso se retienen al
// soltarse) y devuelve los retenidos cuando el tope sube.
func (m *Manager) ajustarPuerta() {
	r := m.retrocesoDe()
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	querido := cap(m.launchGate) - min(r.topeLocked(r.ahora()), cap(m.launchGate))
	for r.retenidos < querido {
		select {
		case m.launchGate <- struct{}{}:
			r.retenidos++
		default:
			return
		}
	}
	for r.retenidos > querido {
		<-m.launchGate
		r.retenidos--
	}
}

// retenerAlSoltar dice si el hueco que se suelta debe quedarse retenido
// porque el tope vigente es menor que la capacidad.
func (m *Manager) retenerAlSoltar() bool {
	r := m.retrocesoDe()
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	querido := cap(m.launchGate) - min(r.topeLocked(r.ahora()), cap(m.launchGate))
	if r.retenidos < querido {
		r.retenidos++
		return true
	}
	return false
}
