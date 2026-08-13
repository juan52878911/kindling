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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/http/pprof"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

// GuestPort es donde se espera que escuche el servidor MCP dentro de la microVM.
const GuestPort = api.GuestPort

// livenessTTL es cuánto se confía en que una instancia sigue viva sin volver a
// preguntárselo al daemon. El vigilante del daemon detecta las muertes en 10 s,
// así que comprobarlo más a menudo desde aquí no aporta nada y sí cuesta.
const livenessTTL = 15 * time.Second

// SessionHeader identifica la conversación MCP. El gateway la usa para enrutar
// SIEMPRE a la misma instancia: el estado de una sesión vive en el proceso del
// servidor MCP, así que mandar la segunda petición a otra microVM la rompería.
const SessionHeader = "Mcp-Session-Id"

// maxProxyBody acota lo que se guarda en memoria para poder reintentar. Coincide
// con el límite que ya aplica el puente al leer una petición.
const maxProxyBody = 8 << 20

// retryKey marca una petición que ya se reintentó, para que el ErrorHandler no
// pueda entrar en bucle consigo mismo.
type retryKey struct{}

func markRetried(ctx context.Context) context.Context {
	return context.WithValue(ctx, retryKey{}, true)
}

func retried(r *http.Request) bool {
	v, _ := r.Context().Value(retryKey{}).(bool)
	return v
}

// readyTimeout acota la espera a que la herramienta abra su puerto. Generoso
// para el arranque en frío, irrelevante tras un thaw.
const readyTimeout = 20 * time.Second

// alive dice si el invitado acepta conexiones AHORA, con un solo intento corto.
//
// El camino sticky lo llama en cada petición, así que tiene que ser barato: en
// LAN, un dial a un puerto abierto es submilisegundo. Lo que detecta es lo que
// el machineID no puede — que el DAEMON congeló la instancia por debajo, sin
// que el gateway tocara su mapa.
func alive(ip string, port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), 400*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

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
	client       *api.Client
	idle         time.Duration
	Ephemeral    bool
	PprofEnabled bool // expone /debug/pprof en Handler(); debe decidirlo el operador
	// freezeFn sustituye la llamada al daemon en los tests. En producción es nil
	// y se usa el cliente: el desalojo por falta de memoria no se puede ejercitar
	// de otro modo sin levantar un daemon con KVM.
	freezeFn func(id string) error
	mu       sync.Mutex
	services map[string]*entry        // servicio -> instancia "por defecto"
	routes   map[string]*sessionRoute // Mcp-Session-Id -> instancia fija
	agg      *aggregator              // endpoint virtual que reúne a todos
	pool     *pool                    // instancias pre-calentadas por servicio
	ensureMu sync.Map                 // servicio -> *sync.Mutex; ver ensure()
	mem      *memory                  // memoria de uso; nil si está desactivada
	pop      *popularity              // popularidad por servicio; guía el prewarm

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

	// inflight cuenta las peticiones que se están atendiendo AHORA.
	//
	// Sin esto, "inactivo" se medía por la LLEGADA de peticiones, y una
	// herramienta que tarda más que g.idle en responder —un analizador estático
	// sobre un árbol grande— se quedaba sin microVM a media faena: el segador la
	// congelaba por debajo del trabajo que estaba corriendo. El cliente veía un
	// "connection timed out" de TCP, que no se parece en nada a la causa.
	//
	// Se toca siempre con g.mu tomado, igual que lastUse.
	inflight int
}

// begin y end marcan el trabajo en vuelo de una instancia.
//
// begin refresca además lastUse: una petición que empieza es uso, aunque tarde
// en terminar. Y end lo vuelve a refrescar, para que el plazo de inactividad se
// cuente desde que la respuesta salió y no desde que la petición entró.
func (g *Gateway) begin(e *entry) {
	g.mu.Lock()
	e.inflight++
	e.lastUse = time.Now()
	g.mu.Unlock()
}

func (g *Gateway) end(e *entry) {
	g.mu.Lock()
	if e.inflight > 0 {
		e.inflight--
	}
	e.lastUse = time.Now()
	g.mu.Unlock()
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
	g.pop = newPopularity(popularityPath())
	return g
}

// Handler expone las rutas del gateway.
//
// El token llega por parámetro en vez de vivir en el Gateway para que arrancar
// sin autenticación sea una decisión explícita de quien compone el servidor: el
// compilador obliga a escribir algo, aunque sea la cadena vacía.
func (g *Gateway) Handler(token string) http.Handler {
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

	// pprof SOLO si el operador lo pidió con -pprof, y SOLO en loopback: eso lo
	// comprueba quien construye el gateway.
	//
	// Queda además detrás de Auth, porque Auth envuelve el mux entero. Que no se
	// le añada nunca una exención como la de /healthz: un volcado de goroutines
	// o la línea de comandos completa no son cosas que deba poder pedir alguien
	// que no tenga el token.
	if g.PprofEnabled {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
		mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
		mux.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))
		mux.Handle("/debug/pprof/block", pprof.Handler("block"))
		mux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
		mux.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))
	}

	// El registro va POR FUERA de la autenticación: los 401 son justo lo que
	// hay que poder ver cuando alguien sondea el puerto.
	return logging(Auth(mux, token))
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

	// La ruta que ve la herramienta no incluye el prefijo de enrutado. Cuando no
	// queda nada detrás del nombre del servicio, la petición va a /mcp: es donde
	// sirve el protocolo un servidor Streamable HTTP nativo, y donde el puente
	// escucha también. Mandarla a "/" solo funcionaba con puente.
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/mcp/"+service)
	if r.URL.Path == "" || r.URL.Path == "/" {
		r.URL.Path = "/mcp"
	}

	// El cuerpo se guarda para poder REENVIARLO una vez. Sin esto, el único
	// reintento posible tras un fallo de conexión es imposible: ReverseProxy ya
	// consumió el original. Las peticiones MCP son JSON de tamaño moderado y el
	// puente ya las acota, así que el coste es asumible.
	if r.Body != nil && r.Method == http.MethodPost {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxProxyBody))
		_ = r.Body.Close()
		if err != nil {
			http.Error(w, "no pude leer el cuerpo", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		r.ContentLength = int64(len(body))
	}

	// Enlace externo: no hay microVM que despertar, se reenvía HTTP al servidor
	// del dueño. Sin este desvío, un servicio registrado con `kling mcp link`
	// respondía 502 "no hay snapshot" porque ensure() lo buscaba en el catálogo
	// de VMs y no lo encontraba.
	if l := g.linkFor(r.Context(), service); l != nil {
		g.handleLinkProxy(w, r, l)
		return
	}

	// A partir de aquí es un servicio respaldado por microVM. Se cuenta la llegada
	// para que el prewarm por popularidad sepa qué se usa de verdad y priorice su
	// fondo antes que el de servicios que nadie llama.
	g.pop.observe(service)

	// Sesión ya conocida: directo a su instancia.
	//
	// Pero antes se COMPRUEBA que esa instancia sigue siendo la de este servicio,
	// y no un cadáver. El estado del gateway y el de las microVMs divergen por
	// tres caminos —el segador congela por TTL, evictLRU hace sitio, ensure la
	// reconstruye— y ninguno tocaba las rutas fijadas. Una sesión soldada a una
	// instancia congelada enrutaba a un invitado pausado: SYN sin respuesta, i/o
	// timeout, y como route() refresca lastUse en cada intento, la ruta no
	// expiraba nunca. Era el cuelgue permanente de context7.
	//
	// La ironía es que congelar preserva la memoria del invitado, así que la
	// sesión del puente SOBREVIVE: basta con descongelar la misma instancia para
	// que siga funcionando.
	if sid := r.Header.Get(SessionHeader); sid != "" {
		if rt := g.route(sid); rt != nil {
			g.mu.Lock()
			e := g.services[rt.service]
			g.mu.Unlock()

			// Dos formas de que la instancia haya muerto bajo la sesión, y hay
			// que cubrir las dos: que el GATEWAY la reconstruyera (cambia el
			// machineID en el mapa) o que el DAEMON la congelara por TTL (el
			// mapa no cambia, pero el invitado deja de responder). Lo segundo no
			// se ve por el machineID, solo comprobando vida.
			if e == nil || e.machineID != rt.machineID || !alive(rt.ip, GuestPort) {
				// La instancia se congeló o se reconstruyó por debajo. Se
				// reconstruye —ensure descongela la misma si sigue warm—.
				// Si el mapa aún cree viva la instancia congelada, se invalida
				// para que ensure la reconstruya en vez de devolverla tal cual.
				g.mu.Lock()
				if cur := g.services[rt.service]; cur != nil && cur.machineID == rt.machineID {
					delete(g.services, rt.service)
				}
				g.mu.Unlock()
				var err error
				e, err = g.ensure(r.Context(), rt.service)
				if err != nil {
					g.forget(sid)
					http.Error(w, fmt.Sprintf("no pude recuperar la sesión de %q: %v", rt.service, err),
						http.StatusBadGateway)
					return
				}
				if e.machineID == rt.machineID {
					// Mismo VMM descongelado: la sesión del puente sigue viva,
					// solo hay que reapuntar al proxy nuevo.
					g.rebind(sid, e)
				} else {
					// Máquina distinta: su puente no conoce esta sesión y
					// responderá 400. Se olvida para que el cliente rehaga el
					// handshake contra la instancia nueva.
					g.forget(sid)
					rt = nil
				}
			}

			if rt != nil {
				g.begin(e)
				defer g.end(e)
				rt.proxy.ServeHTTP(w, r)
				return
			}
		}
		// Sesión desconocida o reasignada: se deja seguir. El puente responderá
		// 400 y el cliente reiniciará el handshake.
	}

	e, err := g.ensure(r.Context(), service)
	if err != nil {
		http.Error(w, fmt.Sprintf("no pude preparar %q: %v", service, err), http.StatusBadGateway)
		return
	}

	// Se observa la respuesta para capturar el Mcp-Session-Id que asigne el
	// puente en el initialize, y fijar desde ahí el enrutado de esa conversación.
	sw := &sessionWriter{ResponseWriter: w, gw: g, service: service, e: e}
	// En defer, no después: un pánico dentro del proxy dejaría inflight alto
	// para siempre, y esa instancia no volvería a congelarse nunca — el segador
	// la respeta precisamente porque cree que está trabajando.
	g.begin(e)
	defer g.end(e)
	e.proxy.ServeHTTP(sw, r)

	// DELETE cierra la sesión: se olvida la ruta para no acumularlas.
	if r.Method == http.MethodDelete {
		if sid := r.Header.Get(SessionHeader); sid != "" {
			g.forget(sid)
		}
	}
}

// handleLinkProxy reenvía la petición HTTP al servidor MCP externo.
//
// El enlace expone la URL completa de su servidor (p. ej. http://host:8080/mcp).
// El cliente, en cambio, pidió `/mcp/<servicio>/...` y ya le quitamos el prefijo
// en el llamador, así que r.URL.Path empieza por `/mcp`. Para que el proxy no
// concatene dos veces la ruta, se elimina el sufijo `/mcp` de la URL del enlace
// antes de construir el destino.
func (g *Gateway) handleLinkProxy(w http.ResponseWriter, r *http.Request, l *api.Link) {
	base := strings.TrimSuffix(l.URL, "/mcp")
	target, err := url.Parse(base)
	if err != nil {
		http.Error(w, fmt.Sprintf("URL de enlace inválida: %v", err), http.StatusInternalServerError)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ServeHTTP(w, r)
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

// rebind reapunta una sesión a la instancia actual de su servicio, conservando
// la sesión: se usa cuando el MISMO VMM se descongeló y solo cambió el proxy.
func (g *Gateway) rebind(sid string, e *entry) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if rt, ok := g.routes[sid]; ok {
		rt.machineID, rt.ip, rt.proxy = e.machineID, e.ip, e.proxy
		rt.lastUse = time.Now()
	}
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
	// No cabe: se hace sitio congelando instancias ociosas y se reintenta,
	// EN BUCLE. Una sola puede no bastar —si el anfitrión está muy justo hacen
	// falta varias—, y rendirse tras la primera dejaba el 502 igual que antes.
	for api.IsInsufficientMemory(err) {
		// No cabe: se hace sitio en vez de rendirse.
		//
		// Un anfitrión justo no puede tener todos los servicios despiertos a la
		// vez, y eso NO debería significar que el último que llega no funcione
		// nunca. Antes fallaba con un 502 perfectamente explicado y perfectamente
		// inútil: el usuario no tiene forma de saber que la solución es esperar a
		// que otro servicio se enfríe.
		//
		// Se congela el más antiguo SIN trabajo en vuelo. Congelar cuesta un par
		// de segundos y descongelar 25 ms, así que la instancia sacrificada
		// vuelve barata; el que espera, en cambio, no tenía alternativa.
		victima := g.evictLRU(ctx, service)
		if victima == "" {
			// No queda nada ocioso que sacrificar: ahora sí hay que rendirse, y
			// el error de falta de memoria explica por qué.
			break
		}
		log.Printf("%s: no cabía; congelé %s para hacerle sitio", service, victima)
		mc, err = g.acquire(ctx, service)
	}
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
	// El dial es corto —o hay alguien escuchando o no lo hay— pero la ESPERA A
	// LA RESPUESTA es larga a propósito: al otro lado hay una herramienta, y una
	// herramienta puede tardar. Un escaneo de semgrep sobre un repo pasa del
	// minuto sin que nada vaya mal, y con 60 s el gateway lo mataba y devolvía
	// un 502 que parecía un fallo del servicio.
	e.proxy.Transport = &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		ResponseHeaderTimeout: 5 * time.Minute,
	}
	e.proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// UNA sola oportunidad, y solo si el cuerpo se puede reenviar.
		//
		// Reintentar llamando otra vez a ServeHTTP con la MISMA petición era una
		// recursión infinita: el cuerpo ya se consumió en el primer intento, así
		// que el reintento falla con "invalid Read on closed Body", eso vuelve a
		// entrar aquí, y así hasta que el cliente se rinde. Medido: 4 min 40 s
		// girando en vacío, con la pila creciendo en cada vuelta.
		//
		// GetBody solo existe si alguien preparó el cuerpo para reenviarlo — lo
		// hace handleProxy. Sin él no hay reintento posible, y fingirlo es peor
		// que fallar.
		// SOLO se reintenta un fallo de DIAL: no había nadie escuchando cuando se
		// intentó conectar. Un timeout de cabeceras es otra cosa —el invitado
		// aceptó la conexión y está trabajando—, y reenviar la petición
		// RE-EJECUTARÍA un tools/call que quizá ya tuvo efecto. La distinción es
		// la misma que en el agregador: "no conecté" se reintenta, "tardó" no.
		var opErr *net.OpError
		esDial := errors.As(err, &opErr) && opErr.Op == "dial"
		if esDial && !retried(r) && r.GetBody != nil && waitReady(r.Context(), e.ip, GuestPort, 3*time.Second) == nil {
			body, berr := r.GetBody()
			if berr == nil {
				log.Printf("proxy %s: %v (un reintento)", service, err)
				r2 := r.Clone(markRetried(r.Context()))
				r2.Body = body
				e.proxy.ServeHTTP(w, r2)
				return
			}
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
		// Las del fondo y las efímeras YA TIENEN DUEÑO, y ese dueño las destruye
		// al terminar. Adoptarlas aquí como instancia persistente crea una
		// máquina con dos dueños: una acción efímera la borra debajo de las
		// sesiones que el gateway había fijado a ella, y esas sesiones mueren
		// sin que nada apunte a la causa.
		if m.Labels["pool"] == "true" || m.Labels["ephemeral"] == "true" {
			return false
		}
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
		From: snap.Name,
		// La política de salida viaja con el snapshot. Sin esto, un servicio
		// importado con -egress internet despierta sin red y cada llamada suya
		// al exterior falla con un "fetch failed" que no señala a ninguna parte.
		Egress:     snap.Egress,
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

// prewarmMarginMiB es el colchón que el prewarm NO toca de la memoria disponible
// del anfitrión: parte para el propio host y parte para el tráfico real —una
// instancia de servicio despertándose fuera del fondo—. Precalentar hasta el
// último MiB dejaría sin aire justo a las peticiones que estamos intentando
// acelerar.
const prewarmMarginMiB = 512

// PrewarmAll rellena el fondo, pero con cabeza: primero los servicios que más se
// usan y solo mientras haya presupuesto de memoria.
//
// Antes rellenaba TODOS los snapshots por igual. Con la RAM justa eso significaba
// precalentar lo que nadie llama y que el servicio popular acabara pagando el 507.
// Ahora se ordena por popularidad (EMA de llegadas) y se para al agotar el
// presupuesto: los menos populares se quedan sin fondo hasta que se libere memoria.
func (g *Gateway) PrewarmAll(ctx context.Context) {
	if g.pool.size == 0 || !g.Ephemeral {
		return
	}
	snaps, err := g.client.Snapshots(ctx)
	if err != nil {
		return
	}

	// Se pliega la popularidad al principio de cada pasada: esta cadencia es la
	// ventana de la media móvil, y de paso deja el historial persistido.
	g.pop.fold()

	type cand struct {
		svc, snap string
		mem       int
	}
	var cands []cand
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
		cands = append(cands, cand{svc: svc, snap: s.Name, mem: s.MemMiB})
	}

	// Orden por popularidad (desc). Estable a propósito: sin historial todos
	// puntúan 0 y se conserva el orden del catálogo —el comportamiento de siempre—,
	// así un gateway recién arrancado no regresa a peor, solo deja de malgastar
	// memoria en lo que nadie usa en cuanto hay datos.
	sort.SliceStable(cands, func(i, j int) bool {
		return g.pop.score(cands[i].svc) > g.pop.score(cands[j].svc)
	})

	// Presupuesto de memoria. Se consulta la memoria disponible del anfitrión (la
	// misma que mira el guard del daemon) menos el margen. La estimación del coste
	// es conservadora —mem_mib entero por copia, sin descontar la compartición de
	// páginas entre instancias del mismo snapshot— para pecar de precavido; el 507
	// limpio de reserveMemory sigue siendo la última red si aun así nos pasamos.
	budget := g.prewarmBudget(ctx) // -1 = no se pudo medir: sin límite
	ready := g.pool.stats()

	for _, c := range cands {
		faltan := g.pool.size - ready[c.svc]
		if faltan <= 0 {
			continue
		}
		if budget >= 0 && c.mem > 0 {
			if budget < c.mem {
				// No cabe ni una instancia más: se acabó el presupuesto y el resto
				// —menos popular, por el orden— se queda sin fondo hasta que se
				// libere memoria.
				break
			}
			if c.mem*faltan > budget {
				faltan = budget / c.mem
			}
			budget -= c.mem * faltan
		}
		g.pool.fillN(ctx, c.svc, c.snap, faltan)
	}
}

// prewarmBudget es cuánta memoria del anfitrión se puede dedicar a precalentar,
// en MiB. Devuelve -1 si no se puede medir (p. ej. un daemon en macOS sin /proc):
// en ese caso no se limita y se cae al comportamiento de rellenarlo todo, con el
// 507 del daemon como única red.
func (g *Gateway) prewarmBudget(ctx context.Context) int {
	ps, err := g.client.ProcStats(ctx)
	if err != nil || ps.AvailableMiB <= 0 {
		return -1
	}
	b := int(ps.AvailableMiB) - prewarmMarginMiB
	if b < 0 {
		b = 0
	}
	return b
}

// Drain destruye las instancias pre-calentadas. Se llama al parar el gateway.
func (g *Gateway) Drain(ctx context.Context) {
	g.pop.fold() // deja el historial de popularidad en disco antes de irse
	g.pool.drain(ctx)
}

func (g *Gateway) reapOnce(ctx context.Context) {
	type victim struct{ service, id string }
	var victims []victim

	g.mu.Lock()
	for svc, e := range g.services {
		// Con trabajo en vuelo NO se congela, por vieja que parezca: lastUse
		// solo dice cuándo llegó algo, no si sigue corriendo.
		if e.inflight == 0 && time.Since(e.lastUse) > g.idle {
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

// evictLRU congela la instancia ociosa más antigua para liberar memoria.
//
// Devuelve el servicio sacrificado, o "" si no había ninguno que sacrificar —en
// cuyo caso quien llama debe rendirse de verdad, porque no hay nada que hacer.
//
// Nunca toca la instancia del servicio que pide sitio (sería absurdo), ni una
// con peticiones en vuelo: congelar debajo de una llamada en curso convierte un
// "espera un poco" en un fallo para alguien que ya estaba siendo atendido.
func (g *Gateway) evictLRU(ctx context.Context, salvo string) string {
	g.mu.Lock()
	var elegido string
	var masAntiguo time.Time
	var id string
	for svc, e := range g.services {
		if svc == salvo || e.inflight > 0 {
			continue
		}
		if elegido == "" || e.lastUse.Before(masAntiguo) {
			elegido, masAntiguo, id = svc, e.lastUse, e.machineID
		}
	}
	if elegido != "" {
		// Se saca del mapa ANTES de congelar: si alguien pide ese servicio
		// mientras tanto, que lo reconstruya en vez de enrutar a una máquina que
		// está a punto de dejar de existir.
		delete(g.services, elegido)
	}
	g.mu.Unlock()

	if elegido == "" {
		return ""
	}

	// El candado del servicio víctima, para no congelar debajo de un ensure
	// concurrente: sin esto, ese ensure no la encuentra en el mapa, hace List(),
	// la ve todavía running (el freeze tarda ~2 s), la adopta como "ya en marcha"
	// y espera 20 s a un invitado que se está pausando. TryLock y no Lock: quien
	// llama a evictLRU ya tiene tomado el candado de SU servicio, y un Lock aquí
	// podría cruzarse con él. Si no se consigue, se prueba la siguiente víctima.
	vlock := g.ensureLock(elegido)
	if !vlock.TryLock() {
		// Ese servicio está ocupado en su propio ensure. Devolverla al mapa y
		// dejar que el llamador reintente con otra: quitarla y no congelarla
		// dejaría al gateway creyendo que no existe.
		return ""
	}
	defer vlock.Unlock()

	freeze := func(id string) error { _, err := g.client.Freeze(ctx, id); return err }
	if g.freezeFn != nil {
		freeze = g.freezeFn
	}
	if err := freeze(id); err != nil {
		log.Printf("no pude congelar %s para hacer sitio: %v", elegido, err)
		return ""
	}
	return elegido
}
