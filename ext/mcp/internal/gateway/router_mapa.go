package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/panico"
)

// QUÉ SERVICIOS TIENE CADA HOST, RECORDADO UNOS SEGUNDOS.
//
// Cada call_tool por /mcp/_all y cada POST sin sesión a /mcp/<servicio>
// preguntaban a TODOS los daemons, uno detrás de otro, qué servicios tienen. Un
// host lento sumaba su retraso a cada petición, y uno caído su plazo entero.
// Ahora se pregunta a todos a la vez y se recuerda la respuesta ttlMapaHosts;
// una búsqueda que no encuentra el servicio vuelve a preguntar (como
// linkRecien), para que un `kling mcp import` recién hecho se vea enseguida.

const (
	ttlMapaHosts = 5 * time.Second

	// Ni forzando se relee más a menudo que esto: una ráfaga de peticiones a
	// un servicio que no existe no debe convertirse en otra contra los daemons.
	pisoMapaHosts = 100 * time.Millisecond

	// Un host que deja de contestar conserva su última lista este tiempo: un
	// fallo pasajero no debe esconder sus servicios. Pasado, deja de ser
	// candidato, como antes, y los duplicados vuelven al siguiente host.
	viejoMapaHosts = 30 * time.Second

	// Plazo de una relectura. Va desligada del contexto de quien la pide:
	// si ese cliente se va a mitad, la lista no debe quedar vacía para todos.
	plazoMapaHosts = 10 * time.Second
)

type catalogoHost struct {
	nombres []string
	okAt    time.Time // última lectura buena; cero = nunca
}

type mapaHosts struct {
	mu    sync.Mutex
	at    time.Time
	hosts []catalogoHost // alineado con Router.hosts; nil = nunca leído
	vuelo chan struct{}  // relectura en curso
}

// catalogos devuelve los servicios de cada host, alineados con r.hosts (nil
// para un host que no contesta). forzar relee aunque esté fresco, salvo que
// la última lectura sea de hace menos de pisoMapaHosts.
func (r *Router) catalogos(ctx context.Context, forzar bool) []catalogoHost {
	m := &r.mapa
	for {
		m.mu.Lock()
		edad := time.Since(m.at)
		if m.hosts != nil && edad < ttlMapaHosts && (!forzar || edad < pisoMapaHosts) {
			out := m.hosts
			m.mu.Unlock()
			return out
		}
		v := m.vuelo
		if v == nil {
			v = make(chan struct{})
			m.vuelo = v
			viejos := m.hosts
			go r.releerCatalogos(ctx, viejos, v)
		}
		m.mu.Unlock()
		select {
		case <-v:
			// Lo que trae es tan fresco como cabe pedir: no se fuerza otra.
			forzar = false
		case <-ctx.Done():
			m.mu.Lock()
			out := m.hosts
			m.mu.Unlock()
			return out
		}
	}
}

// releerCatalogos pregunta a todos los hosts a la vez y guarda la respuesta.
func (r *Router) releerCatalogos(ctx context.Context, viejos []catalogoHost, hecho chan struct{}) {
	m := &r.mapa
	nuevos := make([]catalogoHost, len(r.hosts))
	defer func() {
		m.mu.Lock()
		m.hosts, m.at, m.vuelo = nuevos, time.Now(), nil
		m.mu.Unlock()
		close(hecho)
	}()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), plazoMapaHosts)
	defer cancel()
	var wg sync.WaitGroup
	for i, h := range r.hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			panico.Contener("gateway.router.catalogos", func() {
				names, err := h.GW.agg.cat.services(ctx)
				if err == nil {
					nuevos[i] = catalogoHost{nombres: names, okAt: time.Now()}
					return
				}
				if i < len(viejos) && !viejos[i].okAt.IsZero() && time.Since(viejos[i].okAt) < viejoMapaHosts {
					nuevos[i] = viejos[i]
				}
			})
		}()
	}
	wg.Wait()
}
