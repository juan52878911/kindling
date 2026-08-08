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
}

// warmVM es una microVM restaurada y con su sesión MCP ya abierta.
type warmVM struct {
	id      string
	ip      string
	session string // sesión MCP viva: la llamada se ahorra el initialize
	born    time.Time
}

func newPool(gw *Gateway, size int) *pool {
	return &pool{
		gw: gw, size: size,
		ready:   map[string][]*warmVM{},
		filling: map[string]bool{},
	}
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

// fill repone el fondo de un servicio en segundo plano.
func (p *pool) fill(ctx context.Context, service, snapshot string) {
	p.mu.Lock()
	if p.filling[service] || len(p.ready[service]) >= p.size {
		p.mu.Unlock()
		return
	}
	missing := p.size - len(p.ready[service])
	p.filling[service] = true
	p.mu.Unlock()

	go func() {
		defer func() {
			p.mu.Lock()
			p.filling[service] = false
			p.mu.Unlock()
		}()
		for i := 0; i < missing; i++ {
			vm, err := p.warm(ctx, service, snapshot)
			if err != nil {
				log.Printf("fondo %s: no pude pre-calentar: %v", service, err)
				return
			}
			p.mu.Lock()
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
		// Si el gateway muriera, el daemon las congela en vez de dejarlas vivas.
		TTLSeconds: 600,
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

// drain destruye lo que quede en el fondo. Se llama al parar el gateway: dejar
// microVMs huérfanas sería peor que no haber pre-calentado nada.
func (p *pool) drain(ctx context.Context) {
	p.mu.Lock()
	all := p.ready
	p.ready = map[string][]*warmVM{}
	p.mu.Unlock()

	for _, q := range all {
		for _, vm := range q {
			_ = p.gw.client.Remove(ctx, vm.id)
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
		_ = p.gw.client.Remove(ctx, vm.id)
	}
	if len(dead) > 0 {
		log.Printf("fondo: %d instancia(s) retiradas por antigüedad", len(dead))
	}
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
