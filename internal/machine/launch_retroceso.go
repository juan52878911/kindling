package machine

// RETROCESO DE LA PUERTA.
//
// Si los VMM mueren solos (o una copia restaurada no llega a correr) con la
// puerta alta, lo más probable es una tormenta de KVM en anidado. En vez de
// seguir empujando, el aviso baja el tope a la mitad (mínimo 2) y se
// recupera de uno en uno cada plazoRetroceso sin nuevos avisos.
//
// Pero un solo origen no es una tormenta del host: un invitado con exec que
// hace `reboot -f`, o una plantilla con el agente muerto, producirían avisos
// sin parar y dejarían a todos con el tope mínimo. Por eso:
//   - una máquina ya entregada (salió de la creación) nunca cuenta;
//   - los demás avisos llevan su origen (el dueño o, si no hay, el snapshot) y
//     bajan el tope si hay dos orígenes distintos en ventanaOrigenes, o si los
//     arranques en vuelo son ≥ tope/2 (una tormenta real de un solo inquilino);
//   - un mismo origen baja como mucho un escalón por plazoRetroceso.

import (
	"log"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// plazoRetroceso es lo que dura cada escalón del retroceso.
const plazoRetroceso = 60 * time.Second

// ventanaOrigenes es el margen en que dos avisos de orígenes distintos cuentan
// como una tormenta del host y no como un origen roto.
const ventanaOrigenes = 30 * time.Second

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
	// vistos es el último aviso de cada origen (para la ventana); bajadas, la
	// última vez que cada origen bajó el tope.
	vistos  map[string]time.Time
	bajadas map[string]time.Time
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

// retroceder atiende un aviso del origen dado, con ocupados huecos de la
// puerta tomados (los retenidos se descuentan). Baja el tope a la mitad (sin
// pasar de minPuertaRetroceso ni subirlo) y reinicia el plazo si el aviso
// cumple las condiciones de la cabecera. Devuelve el tope anterior, el nuevo
// y si bajó de verdad.
func (r *retrocesoPuerta) retroceder(origen string, ocupados int) (antes, ahora int, bajo bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.ahora()
	antes = r.topeLocked(t)
	r.cuenta++
	if r.vistos == nil {
		r.vistos, r.bajadas = map[string]time.Time{}, map[string]time.Time{}
	}
	otro := false
	for o, v := range r.vistos {
		if t.Sub(v) > ventanaOrigenes {
			delete(r.vistos, o)
		} else if o != origen {
			otro = true
		}
	}
	r.vistos[origen] = t
	enVuelo := max(ocupados-r.retenidos, 0)
	if !otro && enVuelo*2 < antes {
		return antes, antes, false
	}
	if b, ok := r.bajadas[origen]; ok && t.Sub(b) < plazoRetroceso {
		return antes, antes, false
	}
	for o, b := range r.bajadas {
		if t.Sub(b) >= plazoRetroceso {
			delete(r.bajadas, o)
		}
	}
	r.bajadas[origen] = t
	ahora = min(antes, max(minPuertaRetroceso, antes/2))
	r.activo, r.bajo, r.desde = true, ahora, t
	return antes, ahora, ahora < antes
}

// retrocesos es cuántos avisos ha recibido la puerta.
func (r *retrocesoPuerta) retrocesos() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cuenta
}

// retrocesoDe devuelve el estado de m, creándolo con la capacidad de su puerta.
func (m *Manager) retrocesoDe() *retrocesoPuerta {
	if m.launchGate == nil {
		return nil
	}
	if r := m.retroceso.Load(); r != nil {
		return r
	}
	m.retroceso.CompareAndSwap(nil, &retrocesoPuerta{base: cap(m.launchGate)})
	return m.retroceso.Load()
}

// puertaRetroceder atiende un aviso de la puerta de arranque por motivo.
// entregada: la máquina ya salió de la creación y no cuenta nunca. origen es
// el dueño o, si no hay, el snapshot. Se registra una vez por escalón real.
func (m *Manager) puertaRetroceder(motivo, origen string, entregada bool) {
	if entregada {
		return
	}
	r := m.retrocesoDe()
	if r == nil {
		return
	}
	antes, ahora, bajo := r.retroceder(origen, len(m.launchGate))
	if bajo {
		log.Printf("launch gate: backing off from %d to %d simultaneous boots (%s, origin %q); recovers one step per %s (backoffs so far: %d)",
			antes, ahora, motivo, origen, plazoRetroceso, r.retrocesos())
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

// origenPuerta es el origen de una creación para el retroceso: su dueño o, si
// no hay, el snapshot del que nace.
func origenPuerta(req api.RunRequest) string {
	if o := req.Labels[api.LabelOwner]; o != "" {
		return o
	}
	return req.From
}

// marcarCreando apunta que id se está creando (con su origen) y devuelve la
// función que lo quita; va después del cerrojo de ciclo de vida en el defer,
// así se quita antes de soltarlo.
func (m *Manager) marcarCreando(id, origen string) func() {
	m.creando.Store(id, origen)
	return func() { m.creando.Delete(id) }
}
