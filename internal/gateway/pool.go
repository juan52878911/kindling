package gateway

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

// FONDO DE MÁQUINAS PRE-CALENTADAS.
//
// El modo efímero da una microVM por acción, pero cada llamada paga el ciclo
// completo. Medido con un servidor Go trivial:
//
//	restaurar la microVM      131 ms
//	esperar a la red          53 ms
//	initialize (lanza el hijo) 61 ms   <- con node son 300-500 ms
//	tools/call                 9 ms
//
// Todo salvo el último paso se puede pagar POR ADELANTADO. El fondo mantiene N
// instancias ya restauradas y con su servidor MCP inicializado, esperando. Una
// llamada coge una, la usa, la destruye, y el fondo repone en segundo plano.
//
// Se conserva la semántica efímera —cada acción estrena máquina y la mata al
// terminar— pero el coste visible baja al de la llamada.
type pool struct {
	gw   *Gateway
	size int

	mu    sync.Mutex
	ready map[string][]*warmVM // servicio -> instancias listas
	// filling evita lanzar veinte reposiciones simultáneas del mismo servicio.
	filling map[string]bool
	// closed lo pone drain(). A partir de ahí no se pre-calienta nada nuevo, y
	// lo que estuviera a medio calentar se retira solo en vez de quedar en una
	// cola que ya nadie va a recorrer.
	closed bool

	// wg cuenta las reposiciones en vuelo para que drain pueda esperarlas.
	wg sync.WaitGroup

	// warmFn y removeFn son puntos de inyección para las pruebas: el fondo es
	// casi todo concurrencia —quién espera a quién al apagar—, y eso no se
	// puede ejercitar si cada instancia exige un daemon con KVM detrás.
	// newPool los fija a los de verdad.
	warmFn   func(ctx context.Context, service, snapshot string) (*warmVM, error)
	removeFn func(ctx context.Context, id string) error
}

// warmVM es una microVM restaurada y con su sesión MCP ya abierta.
type warmVM struct {
	id      string
	ip      string
	session string // sesión MCP viva: la llamada se ahorra el initialize
	born    time.Time
}

func newPool(gw *Gateway, size int) *pool {
	p := &pool{
		gw: gw, size: size,
		ready:   map[string][]*warmVM{},
		filling: map[string]bool{},
	}
	p.warmFn = p.warm
	p.removeFn = gw.client.Remove
	return p
}

// take entrega una instancia lista, o nil si el fondo está vacío.
//
// Nunca espera: si no hay nada preparado, quien llama sigue por el camino lento.
// Bloquear aquí convertiría un fondo vacío en latencia añadida sobre la que ya
// hay, que es justo lo contrario de lo que buscamos.
func (p *pool) take(service string) *warmVM {
	p.mu.Lock()
	defer p.mu.Unlock()

	q := p.ready[service]
	if len(q) == 0 {
		return nil
	}
	vm := q[0]
	p.ready[service] = q[1:]
	return vm
}

// fill repone el fondo de un servicio hasta su tamaño, en segundo plano.
func (p *pool) fill(ctx context.Context, service, snapshot string) {
	p.fillN(ctx, service, snapshot, p.size)
}

// fillN repone hasta `want` instancias, sin pasar nunca del tamaño del fondo.
//
// Es lo que permite al prewarm por popularidad precalentar menos de lo que cabría
// cuando el presupuesto de memoria aprieta: el llamador decide cuántas, y aquí
// solo se acota a lo que queda por rellenar.
func (p *pool) fillN(ctx context.Context, service, snapshot string, want int) {
	p.mu.Lock()
	if p.closed || p.filling[service] {
		p.mu.Unlock()
		return
	}
	// Nunca por encima del tamaño del fondo, pida lo que pida el llamador.
	if room := p.size - len(p.ready[service]); want > room {
		want = room
	}
	if want <= 0 {
		p.mu.Unlock()
		return
	}
	missing := want
	p.filling[service] = true
	// wg.Add DENTRO del lock, no después del Unlock.
	//
	// Si se hace fuera, drain() puede colarse en medio: ve el contador a cero,
	// da por hecho que no hay nadie pre-calentando, vacía `ready`, y esta
	// goroutine añade luego su microVM a un mapa que ya nadie va a recorrer.
	// Esa máquina se queda viva hasta que expire su TTL sin que el gateway
	// pueda pedir su Remove. Es además lo que exige el contrato de WaitGroup:
	// un Add que parte de cero tiene que ocurrir antes del Wait.
	p.wg.Add(1)
	p.mu.Unlock()

	go func() {
		defer p.wg.Done()
		defer func() {
			p.mu.Lock()
			p.filling[service] = false
			p.mu.Unlock()
		}()
		for i := 0; i < missing; i++ {
			if err := ctx.Err(); err != nil {
				return
			}
			vm, err := p.warmFn(ctx, service, snapshot)
			if err != nil {
				log.Printf("pool %s: could not prewarm: %v", service, err)
				return
			}
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				// El fondo se vació mientras esta máquina se calentaba: no la
				// va a recoger nadie, así que la retira quien la creó. Con el
				// contexto ya cancelado hay que usar uno nuevo o el Remove se
				// iría sin hacer nada.
				rm, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
				_ = p.removeFn(rm, vm.id)
				cancel()
				return
			}
			p.ready[service] = append(p.ready[service], vm)
			p.mu.Unlock()
		}
	}()
}

// warm restaura una instancia y la deja con la sesión MCP abierta.
func (p *pool) warm(ctx context.Context, service, snapshot string) (*warmVM, error) {
	mc, err := p.gw.client.Run(ctx, api.RunRequest{
		From: snapshot,
		Labels: map[string]string{
			api.LabelService: service,
			"pool":           "true",
		},
		// Red de seguridad para si el gateway muriera: el daemon las congela en
		// vez de dejarlas vivas para siempre.
		//
		// Tiene que dispararse DESPUÉS que la purga del gateway, no antes. Con
		// los 600 s fijos de antes empataba exactamente con evictStale(idle*2)
		// del idle por defecto, y como el daemon cuenta desde StartedAt y el
		// gateway desde que la máquina está lista, el daemon ganaba siempre:
		// congelaba VMs que el fondo seguía creyendo vivas y las entregaba
		// muertas, con el fallo llegando al cliente sin reintento.
		TTLSeconds: int((p.gw.idle*2 + 2*time.Minute).Seconds()),
	})
	if err != nil {
		return nil, err
	}

	base := "http://" + mc.IP + ":" + itoa(GuestPort)
	if err := waitReady(ctx, mc.IP, GuestPort, readyTimeout); err != nil {
		_ = p.gw.client.Remove(context.WithoutCancel(ctx), mc.ID)
		return nil, err
	}
	// El initialize se paga AQUÍ, no cuando llegue la petición.
	sid, err := mcpInit(ctx, base)
	if err != nil {
		_ = p.gw.client.Remove(context.WithoutCancel(ctx), mc.ID)
		return nil, err
	}
	return &warmVM{id: mc.ID, ip: mc.IP, session: sid, born: time.Now()}, nil
}

// drainGrace acota lo que se espera a los pre-calentados en vuelo.
//
// La espera es deseable —una microVM a medio calentar debe poder retirarse
// sola— pero no puede ser incondicional: los fill se lanzan con contextos que
// no se cancelan, así que un Wait desnudo cuelga el apagado del gateway para
// siempre si el daemon deja de responder.
const drainGrace = 5 * time.Second

// drain destruye lo que quede en el fondo. Se llama al parar el gateway: dejar
// microVMs huérfanas sería peor que no haber pre-calentado nada.
func (p *pool) drain(ctx context.Context) {
	// Cerrar PRIMERO: a partir de aquí ningún fill nuevo arranca, y los que
	// estén en vuelo retiran ellos mismos lo que terminen de calentar.
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()

	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		log.Printf("pool: leaving without waiting for prewarmed instances (%v)", ctx.Err())
	case <-time.After(drainGrace):
		log.Printf("pool: prewarmed instances did not finish within %s; continuing cleanup", drainGrace)
	}

	p.mu.Lock()
	all := p.ready
	p.ready = map[string][]*warmVM{}
	p.mu.Unlock()

	for _, q := range all {
		for _, vm := range q {
			_ = p.removeFn(ctx, vm.id)
		}
	}
}

// evictStale retira las instancias que llevan demasiado tiempo esperando.
//
// Una microVM del fondo consume RAM sin hacer nada. Si nadie la reclama, sale
// más barato destruirla y volver a pre-calentar cuando haga falta.
func (p *pool) evictStale(ctx context.Context, maxAge time.Duration) {
	var dead []*warmVM

	p.mu.Lock()
	for svc, q := range p.ready {
		keep := q[:0]
		for _, vm := range q {
			if time.Since(vm.born) > maxAge {
				dead = append(dead, vm)
				continue
			}
			keep = append(keep, vm)
		}
		p.ready[svc] = keep
	}
	p.mu.Unlock()

	for _, vm := range dead {
		_ = p.removeFn(ctx, vm.id)
	}
	if len(dead) > 0 {
		log.Printf("pool: %d instance(s) removed due to age", len(dead))
	}
}

// evictOne retira del fondo la instancia pre-calentada más antigua y la destruye,
// para liberar su RAM. Devuelve true si retiró alguna.
//
// Es el segundo escalón de evictLRU: cuando no hay ninguna instancia de servicio
// que sacrificar, las máquinas del fondo siguen reteniendo memoria sin atender a
// nadie. En un anfitrión justo con el fondo activo, esa RAM es la diferencia
// entre que el servicio que llega arranque o reciba un 507.
func (p *pool) evictOne(ctx context.Context) bool {
	var elegida *warmVM
	var deSvc string

	p.mu.Lock()
	for svc, q := range p.ready {
		if len(q) == 0 {
			continue
		}
		// La más antigua de la cola de cada servicio; entre servicios, la de born
		// menor. Es la que lleva más tiempo esperando sin que nadie la use.
		if elegida == nil || q[0].born.Before(elegida.born) {
			elegida, deSvc = q[0], svc
		}
	}
	if elegida != nil {
		p.ready[deSvc] = p.ready[deSvc][1:]
	}
	p.mu.Unlock()

	if elegida == nil {
		return false
	}
	_ = p.removeFn(ctx, elegida.id)
	log.Printf("pool: removed an instance of %s to make room", deSvc)
	return true
}

func (p *pool) stats() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]int{}
	for svc, q := range p.ready {
		out[svc] = len(q)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
