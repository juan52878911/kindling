// Package gateway enruta llamadas MCP a microVMs bajo demanda.
//
// Es un proceso APARTE del daemon, y a propósito: el daemon nunca escucha en la
// red porque controlarlo equivale a root en su host. El gateway sí escucha, pero
// su superficie es mucho más estrecha — solo sabe despertar instancias de
// snapshots ya existentes y hacer de proxy.
//
//	cliente MCP ──HTTP──> gateway ──socket unix──> daemon ──> microVM
//	                          └────────HTTP proxy───────────────┘
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

// GuestPort es donde se espera que escuche el servidor MCP dentro de la microVM.
const GuestPort = 8080

// livenessTTL es cuánto se confía en que una instancia sigue viva sin volver a
// preguntárselo al daemon. El vigilante del daemon detecta las muertes en 10 s,
// así que comprobarlo más a menudo desde aquí no aporta nada y sí cuesta.
const livenessTTL = 15 * time.Second

// SessionHeader identifica la conversación MCP. El gateway la usa para enrutar
// SIEMPRE a la misma instancia: el estado de una sesión vive en el proceso del
// servidor MCP, así que mandar la segunda petición a otra microVM la rompería.
const SessionHeader = "Mcp-Session-Id"

// readyTimeout acota la espera a que la herramienta abra su puerto. Generoso
// para el arranque en frío, irrelevante tras un thaw.
const readyTimeout = 20 * time.Second

// waitReady sondea el puerto del invitado hasta que acepta conexiones.
func waitReady(ctx context.Context, ip string, port int, timeout time.Duration) error {
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		last = err
		time.Sleep(25 * time.Millisecond)
	}
	return last
}

// Gateway mantiene una instancia caliente por servicio y enruta hacia ella.
type Gateway struct {
	client    *api.Client
	idle      time.Duration
	Ephemeral bool
	mu        sync.Mutex
	services  map[string]*entry        // servicio -> instancia "por defecto"
	routes    map[string]*sessionRoute // Mcp-Session-Id -> instancia fija
	agg       *aggregator              // endpoint virtual que reúne a todos
	pool      *pool                    // instancias pre-calentadas por servicio
	ensureMu  sync.Map                 // servicio -> *sync.Mutex; ver ensure()
	mem       *memory                  // memoria de uso; nil si está desactivada

	// Servidores MCP externos enlazados: no corren aquí, solo se enrutan.
	linkMu    sync.RWMutex
	linkCache []*api.Link
	linkAt    time.Time
}

type entry struct {
	machineID string
	ip        string
	lastUse   time.Time
	proxy     *httputil.ReverseProxy

	// checkedAt es cuándo se confirmó por última vez que la instancia vive.
	checkedAt time.Time
}

// sessionRoute recuerda a qué instancia pertenece cada sesión MCP.
type sessionRoute struct {
	service   string
	machineID string
	ip        string
	proxy     *httputil.ReverseProxy
	lastUse   time.Time
}

func New(client *api.Client, idle time.Duration, ephemeral bool, prewarm int, memService string) *Gateway {
	g := &Gateway{
		client: client, idle: idle, Ephemeral: ephemeral,
		services: map[string]*entry{},
		routes:   map[string]*sessionRoute{},
	}
	g.agg = newAggregator(g, ephemeral)
	g.pool = newPool(g, prewarm)
	g.mem = newMemory(g, memService)
	return g
}

// Handler expone las rutas del gateway.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/services", g.handleServices)
	// El agregador tiene que registrarse ANTES que el comodín de servicio, o
	// "_all" se interpretaría como el nombre de un servicio cualquiera.
	mux.HandleFunc("/mcp/"+AggregatePath, g.handleAggregate)
	mux.HandleFunc("/mcp/"+AggregatePath+"/", g.handleAggregate)
	mux.HandleFunc("/mcp/{service}/", g.handleProxy)
	mux.HandleFunc("/mcp/{service}", g.handleProxy)
	return logging(mux)
}

func logging(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// handleServices lista qué snapshots hay disponibles como servicios MCP.
func (g *Gateway) handleServices(w http.ResponseWriter, r *http.Request) {
	snaps, err := g.client.Snapshots(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, s := range snaps {
		name := s.Name
		if svc := s.Service(); svc != "" {
			name = svc
		}
		status := "frío"
		if n := g.pool.stats()[name]; n > 0 {
			status = fmt.Sprintf("%d instancia(s) pre-calentada(s)", n)
		}
		if e, ok := g.services[name]; ok {
			n := 0
			for _, rt := range g.routes {
				if rt.service == name {
					n++
				}
			}
			status = fmt.Sprintf("caliente en %s · %d sesión(es) · ocioso %s",
				e.ip, n, time.Since(e.lastUse).Round(time.Second))
		}
		fmt.Fprintf(w, "%-24s snapshot=%-20s %s\n", name, s.Name, status)
	}
}

// handleProxy es el camino caliente: asegura instancia y hace de proxy.
//
// Con sesión MCP el enrutado es PEGAJOSO: la misma conversación vuelve siempre a
// la misma microVM, porque su estado vive en el proceso del servidor MCP.
func (g *Gateway) handleProxy(w http.ResponseWriter, r *http.Request) {
	service := r.PathValue("service")
	if service == "" {
		http.Error(w, "falta el servicio en la ruta", http.StatusBadRequest)
		return
	}

	// La ruta que ve la herramienta no incluye el prefijo de enrutado.
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/mcp/"+service)
	if r.URL.Path == "" {
		r.URL.Path = "/"
	}

	// Sesión ya conocida: directo a su instancia, sin consultar al daemon.
	if sid := r.Header.Get(SessionHeader); sid != "" {
		if rt := g.route(sid); rt != nil {
			rt.proxy.ServeHTTP(w, r)
			return
		}
		// Sesión desconocida: puede venir de un gateway anterior. Se deja seguir;
		// el puente responderá 400 y el cliente reiniciará el handshake.
	}

	e, err := g.ensure(r.Context(), service)
	if err != nil {
		http.Error(w, fmt.Sprintf("no pude preparar %q: %v", service, err), http.StatusBadGateway)
		return
	}

	// Se observa la respuesta para capturar el Mcp-Session-Id que asigne el
	// puente en el initialize, y fijar desde ahí el enrutado de esa conversación.
	sw := &sessionWriter{ResponseWriter: w, gw: g, service: service, e: e}
	e.proxy.ServeHTTP(sw, r)

	// DELETE cierra la sesión: se olvida la ruta para no acumularlas.
	if r.Method == http.MethodDelete {
		if sid := r.Header.Get(SessionHeader); sid != "" {
			g.forget(sid)
		}
	}
}

// sessionWriter detecta la cabecera de sesión en la respuesta y registra la ruta.
type sessionWriter struct {
	http.ResponseWriter
	gw      *Gateway
	service string
	e       *entry
	done    bool
}

func (s *sessionWriter) WriteHeader(code int) {
	s.capture()
	s.ResponseWriter.WriteHeader(code)
}

func (s *sessionWriter) Write(b []byte) (int, error) {
	s.capture()
	return s.ResponseWriter.Write(b)
}

// Flush hace falta para que el streaming SSE del puente no se quede atascado.
func (s *sessionWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *sessionWriter) capture() {
	if s.done {
		return
	}
	s.done = true
	if sid := s.Header().Get(SessionHeader); sid != "" {
		s.gw.bind(sid, s.service, s.e)
	}
}

func (g *Gateway) route(sid string) *sessionRoute {
	g.mu.Lock()
	defer g.mu.Unlock()
	rt, ok := g.routes[sid]
	if !ok {
		return nil
	}
	rt.lastUse = time.Now()
	// Mantener viva la instancia: la sesión cuenta como uso del servicio.
	if e, ok := g.services[rt.service]; ok {
		e.lastUse = time.Now()
	}
	return rt
}

func (g *Gateway) bind(sid, service string, e *entry) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.routes[sid] = &sessionRoute{
		service: service, machineID: e.machineID, ip: e.ip,
		proxy: e.proxy, lastUse: time.Now(),
	}
	log.Printf("%s: sesión %s fijada a %s", service, short(sid), e.ip)
}

func (g *Gateway) forget(sid string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.routes, sid)
}

// trunc acorta cadenas para los registros de diagnóstico.
func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// ensure devuelve una instancia viva del servicio, despertándola si hace falta.
//
// Tres casos, de más barato a más caro:
//  1. ya hay una caliente          -> 0 ms
//  2. hay una congelada            -> ~30 ms de thaw
//  3. no hay ninguna               -> se instancia del snapshot dorado
func (g *Gateway) ensure(ctx context.Context, service string) (*entry, error) {
	// Serializado POR SERVICIO. Sin esto, N llamadas concurrentes que llegan
	// antes de existir la instancia entran todas en acquire() y cada una crea la
	// suya: ocho peticiones paralelas levantaban ocho microVMs del mismo servicio
	// persistente, cada una con su proceso de node. El host se quedaba sin RAM y
	// todas acababan agotando su tiempo de espera.
	//
	// Es el mismo patrón que ya protegía la creación de sesión, aplicado un nivel
	// más arriba: quien llega segundo debe esperar y reutilizar, no duplicar.
	lock := g.ensureLock(service)
	lock.Lock()
	defer lock.Unlock()

	g.mu.Lock()
	if e, ok := g.services[service]; ok {
		e.lastUse = time.Now()
		recent := time.Since(e.checkedAt) < livenessTTL
		g.mu.Unlock()

		// Comprobar que sigue viva cuesta un List() al daemon, y List() recorre
		// el disco de TODAS las máquinas para calcular su ocupación. Hacerlo en
		// cada llamada, bajo el candado del servicio, serializaba todo: ocho
		// peticiones paralelas se ponían en fila detrás de ocho recorridos de
		// disco y agotaban su tiempo de espera.
		//
		// Se confía en la instancia durante un rato. Si de verdad murió, la
		// llamada fallará y el manejador de errores del proxy la reconstruye:
		// más barato equivocarse una vez que verificar mil.
		if recent {
			return e, nil
		}
		if g.alive(ctx, e.machineID) {
			g.mu.Lock()
			e.checkedAt = time.Now()
			g.mu.Unlock()
			return e, nil
		}
		// Murió por debajo: se descarta y se reconstruye.
		g.mu.Lock()
		delete(g.services, service)
	}
	g.mu.Unlock()

	mc, err := g.acquire(ctx, service)
	if err != nil {
		return nil, err
	}

	// Tener la microVM en marcha no significa que la herramienta escuche ya. Tras
	// un thaw el proceso está listo casi al instante, pero en frío el invitado aún
	// arranca. Sin esperar aquí, la primera petición se comería un "connection
	// refused" que el cliente MCP interpretaría como que la herramienta no existe.
	if err := waitReady(ctx, mc.IP, GuestPort, readyTimeout); err != nil {
		return nil, fmt.Errorf("la herramienta no empezó a escuchar: %w", err)
	}

	target, _ := url.Parse("http://" + net.JoinHostPort(mc.IP, strconv.Itoa(GuestPort)))
	e := &entry{
		machineID: mc.ID,
		ip:        mc.IP,
		lastUse:   time.Now(),
		checkedAt: time.Now(),
		proxy:     httputil.NewSingleHostReverseProxy(target),
	}
	// Timeouts generosos: MCP usa streaming HTTP y las respuestas pueden ser largas.
	e.proxy.Transport = &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	e.proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// Una herramienta puede reiniciarse entre peticiones. Se le da una
		// oportunidad de volver antes de declarar el fallo.
		if waitReady(r.Context(), e.ip, GuestPort, 3*time.Second) == nil {
			log.Printf("proxy %s: %v (reintentando)", service, err)
			e.proxy.ServeHTTP(w, r)
			return
		}
		log.Printf("proxy %s: %v", service, err)
		http.Error(w, fmt.Sprintf("la herramienta %q no respondió: %v", service, err), http.StatusBadGateway)
	}

	g.mu.Lock()
	g.services[service] = e
	g.mu.Unlock()
	return e, nil
}

// acquire busca una máquina utilizable para el servicio, por orden de coste.
func (g *Gateway) acquire(ctx context.Context, service string) (*api.Machine, error) {
	machines, err := g.client.List(ctx)
	if err != nil {
		return nil, err
	}

	match := func(m *api.Machine) bool {
		return m.Service() == service || m.From == service
	}

	// 1) alguna ya en marcha
	for _, m := range machines {
		if match(m) && m.State == api.StateRunning && m.IP != "" {
			return m, nil
		}
	}
	// 2) alguna congelada: ~30 ms
	for _, m := range machines {
		if match(m) && m.State == api.StateWarm {
			log.Printf("%s: descongelando %s", service, m.Name)
			return g.client.Thaw(ctx, m.ID)
		}
	}
	// 3) instanciar del snapshot dorado
	snap, err := g.snapshotFor(ctx, service)
	if err != nil {
		return nil, err
	}
	log.Printf("%s: instanciando desde el snapshot %s", service, snap.Name)
	return g.client.Run(ctx, api.RunRequest{
		From:       snap.Name,
		Labels:     map[string]string{api.LabelService: service},
		TTLSeconds: int(g.idle.Seconds()) * 2, // red de seguridad si el gateway muere
	})
}

func (g *Gateway) snapshotFor(ctx context.Context, service string) (*api.Snapshot, error) {
	snaps, err := g.client.Snapshots(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range snaps {
		if s.Service() == service || s.Name == service {
			return s, nil
		}
	}
	return nil, fmt.Errorf("no hay snapshot para el servicio %q", service)
}

func (g *Gateway) alive(ctx context.Context, id string) bool {
	machines, err := g.client.List(ctx)
	if err != nil {
		return false
	}
	for _, m := range machines {
		if m.ID == id {
			return m.State == api.StateRunning
		}
	}
	return false
}

// Reap congela las instancias que llevan demasiado tiempo sin recibir peticiones.
//
// Congelar y no eliminar es lo que hace que la siguiente petición cueste 30 ms en
// vez de 2,6 segundos: la herramienta sigue ahí, solo que dormida.
func (g *Gateway) Reap(ctx context.Context) {
	t := time.NewTicker(g.idle / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.reapOnce(ctx)
			g.agg.reap(g.idle * 4) // las sesiones del agregador viven más: son baratas
			g.pool.evictStale(ctx, g.idle*2)
			g.PrewarmAll(ctx)
		}
	}
}

// prewarmAll rellena el fondo de cada servicio conocido.
func (g *Gateway) PrewarmAll(ctx context.Context) {
	if g.pool.size == 0 || !g.Ephemeral {
		return
	}
	snaps, err := g.client.Snapshots(ctx)
	if err != nil {
		return
	}
	for _, s := range snaps {
		// Un servicio con estado usa UNA instancia persistente: pre-calentar
		// varias sería crear grafos paralelos que nadie reconcilia.
		if s.Stateful() {
			continue
		}
		svc := s.Name
		if n := s.Service(); n != "" {
			svc = n
		}
		g.pool.fill(ctx, svc, s.Name)
	}
}

// Drain destruye las instancias pre-calentadas. Se llama al parar el gateway.
func (g *Gateway) Drain(ctx context.Context) { g.pool.drain(ctx) }

func (g *Gateway) reapOnce(ctx context.Context) {
	type victim struct{ service, id string }
	var victims []victim

	g.mu.Lock()
	for svc, e := range g.services {
		if time.Since(e.lastUse) > g.idle {
			victims = append(victims, victim{svc, e.machineID})
			delete(g.services, svc)
		}
	}
	// Las sesiones de una instancia que se congela dejan de ser enrutables: su
	// proceso servidor muere con ella.
	for sid, rt := range g.routes {
		if time.Since(rt.lastUse) > g.idle {
			delete(g.routes, sid)
		}
	}
	g.mu.Unlock()

	for _, v := range victims {
		if _, err := g.client.Freeze(ctx, v.id); err != nil {
			log.Printf("reap %s: %v", v.service, err)
			continue
		}
		log.Printf("%s: congelada por inactividad", v.service)
	}
}

// newSessionID genera el identificador de una sesión del agregador.
func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// links devuelve los servidores externos registrados, cacheados brevemente: el
// agregador los consulta en cada resolución de servicio.
func (g *Gateway) links(ctx context.Context) []*api.Link {
	g.linkMu.RLock()
	if time.Since(g.linkAt) < 30*time.Second {
		out := g.linkCache
		g.linkMu.RUnlock()
		return out
	}
	g.linkMu.RUnlock()

	ls, err := g.client.Links(ctx)
	if err != nil {
		return nil
	}
	g.linkMu.Lock()
	g.linkCache, g.linkAt = ls, time.Now()
	g.linkMu.Unlock()
	return ls
}

// linkFor busca el enlace que sirve a un servicio, o nil si lo sirve una microVM.
func (g *Gateway) linkFor(ctx context.Context, service string) *api.Link {
	for _, l := range g.links(ctx) {
		if l.Service() == service || l.Name == service {
			return l
		}
	}
	return nil
}

// ensureLock devuelve el candado de aprovisionamiento de un servicio.
func (g *Gateway) ensureLock(service string) *sync.Mutex {
	v, _ := g.ensureMu.LoadOrStore(service, &sync.Mutex{})
	return v.(*sync.Mutex)
}
