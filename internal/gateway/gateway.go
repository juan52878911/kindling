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
	"net/http/httptest"
	"net/http/httputil"
	"net/http/pprof"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/internal/api"

	"github.com/juan52878911/kindling/internal/panico"
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
	// KeepWarm mantiene caliente la primaria de los N servicios más populares en
	// modo PERSISTENTE, para que la primera sesión no pague el arranque en frío
	// (~16 s bajo KVM anidado en Mac; ~3 s en Linux nativo). 0 = desactivado (por
	// defecto). Lo decide el operador, igual que PprofEnabled: en un host con la RAM
	// justa (la caja x86 de pruebas) mantener instancias que nadie usa provoca
	// desalojos, así que es una palanca para hosts donde el cuello es el ARRANQUE y
	// no la memoria. A diferencia del prewarm (fondo efímero), esto puebla
	// g.services normales: la siguiente petición reutiliza la instancia caliente, o
	// —si el segador la congeló por ociosa— paga solo un thaw (~25 ms).
	KeepWarm int
	// freezeFn sustituye la llamada al daemon en los tests. En producción es nil
	// y se usa el cliente: el desalojo por falta de memoria no se puede ejercitar
	// de otro modo sin levantar un daemon con KVM.
	freezeFn func(id string) error
	mu       sync.Mutex
	services map[string]*entry        // servicio -> instancia "por defecto" (primaria)
	extra    map[string][]*entry      // servicio -> RÉPLICAS de scale-out (además de la primaria)
	routes   map[string]*sessionRoute // Mcp-Session-Id -> instancia fija
	agg      *aggregator              // endpoint virtual que reúne a todos
	pool     *pool                    // instancias pre-calentadas por servicio
	ensureMu sync.Map                 // servicio -> *sync.Mutex; ver ensure()

	// Último estado de salud escrito por servicio, para no repetir la escritura
	// en cada petición. Ver anotarSalud.
	saludMu    sync.Mutex
	saludVista map[string]bool
	mem        *memory     // memoria de uso; nil si está desactivada
	pop        *popularity // popularidad por servicio; guía el prewarm

	// Cuotas por tenant (token con nombre). Reparto justo, NO seguridad: ver
	// quota.go. extraTenants son los tokens con nombre; tenantInflight lleva la
	// cuenta de peticiones en vuelo por tenant, bajo su propio candado para no
	// pelear con g.mu en el camino caliente.
	extraTenants   []TenantLimit
	tenantMu       sync.Mutex
	tenantInflight map[string]int

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

	// tenant es a quién se atribuye esta instancia despierta: el primer tenant
	// que la despertó o creó. Guía la cuota de instancias y el fairness de
	// evictLRU (un tenant sacrifica lo suyo antes que lo ajeno). Vacío = sin
	// dueño conocido (p. ej. adoptada de un arranque anterior). Se toca con g.mu.
	tenant string

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

	// maxSessions es cuántas sesiones (procesos servidor) caben en ESTA instancia,
	// derivado de su memoria con la misma fórmula que el puente (ver gwMaxSessions).
	// Cuando las sesiones de un servicio superan la capacidad de una instancia, el
	// gateway crea RÉPLICAS (g.extra) y reparte: es lo que permite usar la misma
	// herramienta en paralelo. Se fija al crear la instancia y no cambia.
	maxSessions int
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
		extra:    map[string][]*entry{},
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
	// hay que poder ver cuando alguien sondea el puerto. authHandler resuelve el
	// token a un tenant (el único = "default") y lo cuelga del contexto para que
	// handleProxy pueda aplicar las cuotas.
	return logging(g.authHandler(mux, token))
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
		status := "cold"
		if n := g.pool.stats()[name]; n > 0 {
			status = fmt.Sprintf("%d prewarmed instance(s)", n)
		}
		if e, ok := g.services[name]; ok {
			n := 0
			for _, rt := range g.routes {
				if rt.service == name {
					n++
				}
			}
			status = fmt.Sprintf("warm at %s · %d session(s) · idle %s",
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
		http.Error(w, "missing service in path", http.StatusBadRequest)
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
			http.Error(w, "could not read body", http.StatusBadRequest)
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

	// Un GET SIN sesión es la sonda del "stream SSE independiente" del transporte
	// Streamable HTTP: el cliente pregunta si el servidor le empujará mensajes por
	// su cuenta. Nuestro puente no ofrece ese stream sin una sesión previa, y
	// devolvía 404 —que clientes como opencode interpretan como "servidor caído" y
	// abandonan la conexión, así que un `kling connect <servicio>` / `migrate` no
	// llegaba a cargar—. El spec de MCP dice que un endpoint sin ese stream debe
	// responder 405; entonces el cliente cae a POST y conecta. Se responde aquí,
	// ANTES de despertar la microVM: una sonda no debe costar un thaw. El GET CON
	// sesión sí sigue (abre el stream de esa conversación en el puente).
	if r.Method == http.MethodGet && r.Header.Get(SessionHeader) == "" {
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "this endpoint does not offer a standalone SSE stream; use POST",
			http.StatusMethodNotAllowed)
		return
	}

	// A partir de aquí es un servicio respaldado por microVM. Se cuenta la llegada
	// para que el prewarm por popularidad sepa qué se usa de verdad y priorice su
	// fondo antes que el de servicios que nadie llama.
	g.pop.observe(service)

	// Cuota de peticiones en vuelo del tenant. Es reparto justo, NO seguridad
	// (ver quota.go): un solo cliente en bucle no debe acaparar todas las
	// conexiones y dejar a los demás esperando. El 429 envuelve TODA la petición
	// —tanto el camino pegajoso como el que despierta instancia— con un único
	// begin/end, así que basta contabilizar aquí una vez.
	tnt := tenantFrom(r.Context())
	if !g.tenantBegin(tnt) {
		http.Error(w, fmt.Sprintf(
			"429: tenant %q has reached its quota of %d in-flight requests.\n"+
				"This is a fair-share limit, not a security limit: retry once the earlier ones finish.",
			tnt.name, tnt.maxInflight), http.StatusTooManyRequests)
		return
	}
	defer g.tenantEnd(tnt)

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
			// La instancia de la sesión puede ser la primaria O una réplica de
			// scale-out: se busca por machineID entre todas, no solo la primaria
			// (mirar solo g.services rompía las sesiones enrutadas a una réplica).
			g.mu.Lock()
			e := g.entryByMachineLocked(rt.service, rt.machineID)
			g.mu.Unlock()

			// Dos formas de que la instancia haya muerto bajo la sesión: que el
			// GATEWAY la retirara (ya no aparece por machineID) o que el DAEMON la
			// congelara por TTL (aún figura, pero el invitado no responde). Lo
			// segundo solo se ve comprobando vida.
			if e == nil || !alive(rt.ip, GuestPort) {
				// Se invalida la instancia congelada (si aún figura) para que
				// ensure la reconstruya en vez de devolverla tal cual, y se
				// reconstruye la primaria del servicio.
				g.mu.Lock()
				g.removeEntryLocked(rt.service, rt.machineID)
				g.mu.Unlock()
				var err error
				e, err = g.ensure(r.Context(), rt.service)
				if err != nil {
					g.forget(sid)
					if errors.Is(err, errTenantInstances) {
						http.Error(w, fmt.Sprintf("could not recover session for %q: %v", rt.service, err),
							http.StatusTooManyRequests)
						return
					}
					http.Error(w, fmt.Sprintf("could not recover session for %q: %v", rt.service, err),
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

	// Sesión NUEVA (initialize): se coloca en una instancia con hueco, escalando a
	// una réplica si todas están llenas. Es lo que permite el uso en paralelo.
	g.serveNewSession(w, r, service, tnt)

	// DELETE cierra la sesión: se olvida la ruta para no acumularlas.
	if r.Method == http.MethodDelete {
		if sid := r.Header.Get(SessionHeader); sid != "" {
			g.forget(sid)
		}
	}
}

// maxScaleOut acota cuántas réplicas se crean para un servicio en una ráfaga de
// sesiones nuevas. Es un cortacircuitos: la cuota de instancias del tenant y la
// memoria del host ya limitan antes; esto solo evita un bucle si el puente
// devolviera "lleno" para siempre por un motivo inesperado.
const maxScaleOut = 16

// serveNewSession atiende una petición SIN sesión previa —típicamente el
// initialize de una conversación nueva—: la coloca en una instancia con hueco, y
// si todas están llenas crea una RÉPLICA y reintenta. N sesiones concurrentes de
// la misma herramienta acaban en varias instancias, que es lo que las hace
// usables en paralelo.
//
// La respuesta se BUFEREA para poder reintentar sin habérsela mandado ya al
// cliente. Solo se buferea AQUÍ, donde la respuesta es un initialize pequeño; las
// peticiones de una sesión ya establecida (tools/call, que pueden devolver mucho
// o en streaming) siguen yendo directas por el camino pegajoso.
func (g *Gateway) serveNewSession(w http.ResponseWriter, r *http.Request, service string, tnt *tenant) {
	// Sin cuerpo reenviable no hay reintento posible: se sirve directo por la
	// primaria. handleProxy prepara GetBody para los POST, así que esto solo pasa
	// con métodos sin cuerpo, que no crean sesión y por tanto nunca dan el 400 de
	// tope.
	if r.GetBody == nil {
		e, err := g.ensure(r.Context(), service)
		if err != nil {
			g.newSessionError(w, service, err)
			return
		}
		g.anotarExito(service)
		g.begin(e)
		defer g.end(e)
		e.proxy.ServeHTTP(w, r)
		return
	}

	for try := 0; try < maxScaleOut; try++ {
		var e *entry
		var err error
		if try == 0 {
			e, err = g.pickInstance(r.Context(), service, tnt)
		} else {
			// El intento anterior chocó con el tope de una instancia: se fuerza una
			// réplica nueva para esta sesión.
			e, err = g.scaleOut(r.Context(), service, tnt)
		}
		if err != nil {
			g.newSessionError(w, service, err)
			return
		}
		g.anotarExito(service)

		if b, berr := r.GetBody(); berr == nil {
			r.Body = b
		}
		rec := httptest.NewRecorder()
		g.begin(e)
		e.proxy.ServeHTTP(rec, r)
		g.end(e)

		// ¿El puente rechazó por tope de sesiones? Esa instancia está llena: se crea
		// otra y se reintenta. Cualquier otra respuesta (incluido otro 400) se
		// entrega tal cual al cliente.
		if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "session limit") {
			continue
		}

		// Entregar la respuesta bufereada y fijar la ruta de la sesión a ESTA
		// instancia (primaria o réplica), para que las siguientes peticiones de la
		// conversación vuelvan aquí.
		for k, vs := range rec.Header() {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		if sid := rec.Header().Get(SessionHeader); sid != "" {
			g.bind(sid, service, e)
		}
		return
	}
	http.Error(w, fmt.Sprintf("could not place session for %q: all replicas full or no room on host", service),
		http.StatusServiceUnavailable)
}

func (g *Gateway) newSessionError(w http.ResponseWriter, service string, err error) {
	// La cuota de instancias del tenant es un 429 (reparto justo), no un 502: el
	// servicio no falla, es que este tenant ya tiene todas las suyas.
	if errors.Is(err, errTenantInstances) {
		http.Error(w, fmt.Sprintf("could not prepare %q: %v", service, err), http.StatusTooManyRequests)
		return
	}
	g.anotarFallo(service, err)
	http.Error(w, fmt.Sprintf("could not prepare %q: %v", service, err), http.StatusBadGateway)
}

// anotarFallo guarda en el snapshot que este servicio no se pudo preparar.
//
// El caso que lo motiva: nueve servicios estuvieron 26 HORAS caídos —sus snapshots
// habían quedado irrestaurables tras reiniciar el host— mientras `kling status`
// informaba «services: ✓ 9» y la columna HEALTH decía «not probed». El fallo se
// veía en cada petición y no se guardaba en ningún sitio.
//
// Es mejor señal que un sondeo periódico y no cuesta nada: un sondeo activo
// levanta una microVM por servicio y por vuelta, y sólo mira cuando le toca;
// esto refleja lo que de verdad le pasa a quien usa el servicio, en el momento
// en que le pasa.
//
// En segundo plano y sin bloquear la respuesta al cliente: el error ya está
// decidido, y si anotarlo falla no hay nada mejor que hacer que registrarlo.
// saludCambio registra el estado nuevo y dice si difiere del último anotado.
//
// Vive aparte de anotarSalud para poder comprobarse: la decisión de escribir es
// la parte con lógica, y la escritura es un viaje al daemon dentro de una
// goroutine que un test no debería tener que montar.
func (g *Gateway) saludCambio(service string, sano bool) bool {
	g.saludMu.Lock()
	defer g.saludMu.Unlock()
	if previo, hay := g.saludVista[service]; hay && previo == sano {
		return false
	}
	if g.saludVista == nil {
		g.saludVista = map[string]bool{}
	}
	g.saludVista[service] = sano
	return true
}

func (g *Gateway) anotarExito(service string) {
	g.anotarSalud(service, true, "")
}

func (g *Gateway) anotarFallo(service string, causa error) {
	g.anotarSalud(service, false, causa.Error())
}

// anotarSalud escribe en el meta del snapshot SOLO cuando el estado cambia.
//
// Sin esa memoria haría una escritura a disco por petición atendida, que es
// justo lo que no puede permitirse un camino caliente. Con ella, un servicio
// sano no cuesta nada y uno que se rompe (o se recupera) lo anota una vez.
//
// El recuerdo vive en memoria: tras reiniciar el gateway, la primera petición de
// cada servicio vuelve a escribir su estado. Es barato y deja el meta al día
// aunque algo lo cambiara mientras estaba parado.
func (g *Gateway) anotarSalud(service string, sano bool, causa string) {
	if !g.saludCambio(service, sano) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := g.client.SetHealth(ctx, service, sano, causa); err != nil {
			log.Printf("%s: couldn't record its health in the snapshot: %v", service, err)
		}
	}()
}

// pickInstance devuelve una instancia del servicio con hueco de sesión. Despierta
// la primaria si hace falta, y si todas las instancias (primaria + réplicas) están
// al tope, crea una réplica nueva.
func (g *Gateway) pickInstance(ctx context.Context, service string, tnt *tenant) (*entry, error) {
	if _, err := g.ensure(ctx, service); err != nil {
		return nil, err
	}
	g.mu.Lock()
	for _, e := range g.entriesLocked(service) {
		if g.sessionCountLocked(e.machineID) < e.maxSessions {
			g.mu.Unlock()
			return e, nil
		}
	}
	g.mu.Unlock()
	return g.scaleOut(ctx, service, tnt)
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
		http.Error(w, fmt.Sprintf("invalid link URL: %v", err), http.StatusInternalServerError)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ServeHTTP(w, r)
}

func (g *Gateway) route(sid string) *sessionRoute {
	g.mu.Lock()
	defer g.mu.Unlock()
	rt, ok := g.routes[sid]
	if !ok {
		return nil
	}
	rt.lastUse = time.Now()
	// Mantener viva la instancia CONCRETA de esta sesión —primaria o réplica—: si
	// se refrescara solo la primaria, el segador congelaría una réplica con
	// sesiones activas por debajo.
	if e := g.entryByMachineLocked(rt.service, rt.machineID); e != nil {
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
	log.Printf("%s: session %s bound to %s", service, short(sid), e.ip)
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

	tnt := tenantFrom(ctx)

	g.mu.Lock()
	if e, ok := g.services[service]; ok {
		e.lastUse = time.Now()
		// La instancia ya está caliente: no se despierta nada nuevo, así que no
		// se aplica la cuota de instancias. Si no tenía dueño, lo adopta el primer
		// tenant que la usa —importa para el fairness de evictLRU—.
		if e.tenant == "" {
			e.tenant = tnt.name
		}
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

	// A partir de aquí se DESPIERTA/CREA la instancia primaria del servicio.
	e, err := g.buildEntry(ctx, service, tnt, false)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.services[service] = e
	g.mu.Unlock()
	return e, nil
}

// buildEntry despierta o crea UNA instancia del servicio y la devuelve montada
// (con su proxy), SIN registrarla: quien llama decide si es la primaria
// (g.services) o una réplica de scale-out (g.extra). Aplica siempre la cuota de
// instancias del tenant y el desalojo por memoria, porque siempre crea/despierta
// —el camino de reutilizar una instancia caliente retorna antes de llegar aquí—.
func (g *Gateway) buildEntry(ctx context.Context, service string, tnt *tenant, fresh bool) (*entry, error) {
	// Cuota de instancias del tenant: reparto justo, no seguridad (ver quota.go).
	if tnt.maxInstances > 0 {
		g.mu.Lock()
		actuales := g.tenantInstances(tnt.name)
		g.mu.Unlock()
		if actuales >= tnt.maxInstances {
			return nil, fmt.Errorf("%w: %q already has %d instance(s) awake (max %d)",
				errTenantInstances, tnt.name, actuales, tnt.maxInstances)
		}
	}

	mc, err := g.acquire(ctx, service, fresh)
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
		victima := g.evictLRU(ctx, service, tnt.name)
		if victima == "" {
			// No queda nada ocioso que sacrificar: ahora sí hay que rendirse, y
			// el error de falta de memoria explica por qué.
			break
		}
		log.Printf("%s: didn't fit; froze %s to make room", service, victima)
		mc, err = g.acquire(ctx, service, fresh)
	}
	if err != nil {
		return nil, err
	}

	// Tener la microVM en marcha no significa que la herramienta escuche ya. Tras
	// un thaw el proceso está listo casi al instante, pero en frío el invitado aún
	// arranca. Sin esperar aquí, la primera petición se comería un "connection
	// refused" que el cliente MCP interpretaría como que la herramienta no existe.
	wr0 := time.Now()
	if err := waitReady(ctx, mc.IP, GuestPort, readyTimeout); err != nil {
		return nil, fmt.Errorf("tool did not start listening: %w", err)
	}
	// Cuánto tardó el 8080 en aceptar es la métrica que discrimina el cuello de
	// botella del arranque (ver docs de rendimiento en Mac): un thaw acepta casi al
	// instante, un arranque en frío bajo KVM anidado tarda segundos. Se registra solo
	// cuando es notable, para no ensuciar el log con los thaws de milisegundos.
	if d := time.Since(wr0); d > 500*time.Millisecond {
		log.Printf("%s: waitReady %v (fresh=%v)", service, d.Round(time.Millisecond), fresh)
	}

	target, _ := url.Parse("http://" + net.JoinHostPort(mc.IP, strconv.Itoa(GuestPort)))
	e := &entry{
		machineID:   mc.ID,
		ip:          mc.IP,
		lastUse:     time.Now(),
		checkedAt:   time.Now(),
		tenant:      tnt.name, // quien la despertó es su dueño para cuota y fairness
		maxSessions: gwMaxSessions(mc.MemMiB),
		proxy:       httputil.NewSingleHostReverseProxy(target),
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
				log.Printf("proxy %s: %v (one retry)", service, err)
				r2 := r.Clone(markRetried(r.Context()))
				r2.Body = body
				e.proxy.ServeHTTP(w, r2)
				return
			}
		}
		log.Printf("proxy %s: %v", service, err)
		http.Error(w, fmt.Sprintf("tool %q did not respond: %v", service, err), http.StatusBadGateway)
	}

	return e, nil
}

// gwMaxSessions calcula cuántas sesiones caben en una instancia según su memoria.
// Réplica de deriveMaxSessions del puente (cmd/kling-bridge/sessions.go): 64 MiB
// por sesión, 192 reservados, tope 32, mínimo 1. Sirve para saber CUÁNDO escalar
// sin preguntárselo al invitado; si por drift se equivoca, el 400 del puente lo
// corrige (handleProxy crea otra réplica). Si cambia la fórmula del puente, hay
// que cambiar esta —el 400 lo salva, pero mejor no depender de él—.
func gwMaxSessions(memMiB int) int {
	const sessionMiB, reservedMiB, cap = 64, 192, 32
	if memMiB <= 0 {
		return 1
	}
	usable := memMiB - reservedMiB
	if usable < sessionMiB {
		return 1
	}
	if n := usable / sessionMiB; n <= cap {
		return n
	}
	return cap
}

// entriesLocked devuelve TODAS las instancias vivas de un servicio: la primaria y
// las réplicas de scale-out. Se llama con g.mu tomado.
func (g *Gateway) entriesLocked(service string) []*entry {
	var es []*entry
	if e := g.services[service]; e != nil {
		es = append(es, e)
	}
	es = append(es, g.extra[service]...)
	return es
}

// entryByMachineLocked encuentra la instancia (primaria o réplica) de un servicio
// por su machineID. Es lo que permite que una sesión pegajosa a una RÉPLICA
// vuelva a ella y no se confunda con la primaria. Se llama con g.mu tomado.
func (g *Gateway) entryByMachineLocked(service, machineID string) *entry {
	for _, e := range g.entriesLocked(service) {
		if e.machineID == machineID {
			return e
		}
	}
	return nil
}

// sessionCountLocked cuenta cuántas sesiones vivas hay enrutadas a una instancia,
// derivándolo del mapa de rutas (fuente autoritativa, sin contador que se
// desincronice). Se llama con g.mu tomado.
func (g *Gateway) sessionCountLocked(machineID string) int {
	n := 0
	for _, rt := range g.routes {
		if rt.machineID == machineID {
			n++
		}
	}
	return n
}

// removeEntryLocked quita una instancia del servicio, sea la primaria o una
// réplica. Se llama con g.mu tomado.
func (g *Gateway) removeEntryLocked(service, machineID string) {
	if e := g.services[service]; e != nil && e.machineID == machineID {
		delete(g.services, service)
		return
	}
	es := g.extra[service]
	for i, e := range es {
		if e.machineID == machineID {
			g.extra[service] = append(es[:i], es[i+1:]...)
			if len(g.extra[service]) == 0 {
				delete(g.extra, service)
			}
			return
		}
	}
}

// scaleOut crea una RÉPLICA nueva del servicio para absorber sesiones que no caben
// en las instancias existentes. Es lo que hace usable una herramienta en paralelo:
// cada réplica atiende hasta su maxSessions, y el ruteo pegajoso manda cada sesión
// a la suya. NO toma el candado por-servicio de ensure: queremos que varias
// réplicas puedan nacer a la vez para sesiones concurrentes.
func (g *Gateway) scaleOut(ctx context.Context, service string, tnt *tenant) (*entry, error) {
	e, err := g.buildEntry(ctx, service, tnt, true)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.extra[service] = append(g.extra[service], e)
	total := 1 + len(g.extra[service])
	g.mu.Unlock()
	log.Printf("%s: scale-out — new replica %s (%d instances of the service)", service, short(e.machineID), total)
	return e, nil
}

// acquire busca una máquina utilizable para el servicio, por orden de coste.
// acquire busca/crea una máquina para el servicio. Con fresh=true SIEMPRE crea una
// instancia nueva del snapshot dorado, sin reutilizar una que ya esté en marcha:
// es lo que necesita el scale-out, porque reutilizar la instancia llena sería
// volver a chocar con su tope de sesiones. Con fresh=false (la primaria) reutiliza
// por orden de coste.
func (g *Gateway) acquire(ctx context.Context, service string, fresh bool) (*api.Machine, error) {
	if fresh {
		return g.runFresh(ctx, service)
	}
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
			log.Printf("%s: thawing %s", service, m.Name)
			return g.client.Thaw(ctx, m.ID)
		}
	}
	// 3) instanciar del snapshot dorado
	return g.runFresh(ctx, service)
}

// runFresh crea una instancia NUEVA del servicio desde su snapshot dorado. Cada
// llamada da una microVM distinta (su propio machineID), que es lo que permite
// tener varias réplicas del mismo servicio a la vez.
func (g *Gateway) runFresh(ctx context.Context, service string) (*api.Machine, error) {
	snap, err := g.snapshotFor(ctx, service)
	if err != nil {
		return nil, err
	}
	log.Printf("%s: instantiating from snapshot %s", service, snap.Name)
	return g.client.Run(ctx, api.RunRequest{
		From: snap.Name,
		// La política de salida viaja con el snapshot. Sin esto, un servicio
		// importado con -egress internet despierta sin red y cada llamada suya
		// al exterior falla con un "fetch failed" que no señala a ninguna parte.
		Egress: snap.Egress,
		// El techo de CPU también viaja con el snapshot: sin esto la restauración
		// caía al defaultCPUPct=50 del daemon y un servicio importado con más CPU
		// arrancaba estrangulado. 0 (snapshots viejos) deja que el daemon decida.
		CPUPct:     snap.CPUPct,
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
	return nil, fmt.Errorf("no snapshot for service %q", service)
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
			// El segador toca cinco subsistemas por vuelta. Un panico en
			// cualquiera de ellos mataria el gateway entero, y con el se
			// caerian TODOS los servicios a la vez — incluidos los que no
			// tienen nada que ver con el que fallo.
			panico.Contener("gateway.Reap", func() {
				g.reapOnce(ctx)
				g.agg.reap(g.idle * 4) // las sesiones del agregador viven más: son baratas
				g.pool.evictStale(ctx, g.idle*2)
				g.PrewarmAll(ctx)
				g.KeepWarmAll(ctx)
			})
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

// KeepWarmAll mantiene caliente la primaria de los servicios más populares en modo
// PERSISTENTE, para que la primera sesión no pague el arranque en frío. Es la
// hermana persistente de PrewarmAll: aquel llena el fondo EFÍMERO (instancias de un
// solo uso, consumidas por callEphemeral), y encima se apaga si !Ephemeral; este
// puebla g.services normales vía ensure(), que es de donde tiran las sesiones
// persistentes. Sin esto, el primer initialize de un servicio dormido entra en
// buildEntry→acquire→runFresh y se come los ~16 s de arranque en frío (Mac); con
// esto, la instancia ya existe (reutilización directa) o el segador la dejó WARM y
// solo se paga un thaw (~25 ms).
//
// Apagado por defecto (KeepWarm == 0): en un host con la RAM justa mantener
// calientes servicios que nadie llama fuerza desalojos. Es una palanca para hosts
// donde el cuello es el arranque, no la memoria.
//
// Deja que el segador la congele por ociosa: no se ancla. La siguiente pasada la
// re-asegura (reutiliza la RUNNING, o descongela la WARM), nunca vuelve a runFresh.
// Así la RAM sigue siendo honesta en hosts justos y aun así se elimina el arranque
// en frío del camino crítico.
func (g *Gateway) KeepWarmAll(ctx context.Context) {
	if g.KeepWarm <= 0 {
		return
	}
	snaps, err := g.client.Snapshots(ctx)
	if err != nil {
		return
	}

	// La popularidad solo avanza si alguien la pliega una vez por pasada. En modo
	// persistente PrewarmAll sale antes de plegarla (el gate !Ephemeral), así que lo
	// hace este; si además corriera el efímero, PrewarmAll ya la plegó y no se
	// re-pliega para no adelantar dos veces la ventana de la media móvil.
	if !g.Ephemeral {
		g.pop.fold()
	}

	type cand struct {
		svc string
		mem int
	}
	var cands []cand
	for _, s := range snaps {
		// Un servicio con estado usa UNA instancia persistente; ya la mantiene
		// caliente su propio uso, no se fuerza aquí.
		if s.Stateful() {
			continue
		}
		svc := s.Name
		if n := s.Service(); n != "" {
			svc = n
		}
		cands = append(cands, cand{svc: svc, mem: s.MemMiB})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		return g.pop.score(cands[i].svc) > g.pop.score(cands[j].svc)
	})

	budget := g.prewarmBudget(ctx) // -1 = no se pudo medir: sin límite
	warmed := 0
	for _, c := range cands {
		if warmed >= g.KeepWarm {
			break
		}
		// ¿Ya está caliente la primaria? No cuesta nada y cuenta para el cupo.
		g.mu.Lock()
		_, yaCaliente := g.services[c.svc]
		g.mu.Unlock()
		if yaCaliente {
			warmed++
			continue
		}
		// Presupuesto: no despertar una primaria nueva si no cabe. La estimación es
		// conservadora (mem_mib entero, sin descontar páginas compartidas), igual que
		// en PrewarmAll; el 507 del daemon sigue siendo la última red.
		if budget >= 0 && c.mem > 0 {
			if budget < c.mem {
				break
			}
			budget -= c.mem
		}
		// ensure() hace todo el trabajo pesado: candado por servicio, buildEntry y
		// registro en g.services. En una goroutine para no bloquear el segador ~16 s
		// por cada servicio en frío; la compuerta de arranque del daemon serializa los
		// encendidos de verdad, así que disparar varias a la vez es seguro. ensure es
		// idempotente bajo su candado, de modo que si la siguiente pasada la encuentra
		// aún arrancando no duplica nada. Contexto de fondo sin tenant: es una acción
		// del sistema, el primer usuario real la adopta como dueña (ver ensure).
		warmed++
		go func(svc string) {
			if _, err := g.ensure(ctx, svc); err != nil {
				log.Printf("keepwarm %s: %v", svc, err)
				return
			}
			log.Printf("%s: primary kept warm", svc)
		}(c.svc)
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
	// Con trabajo en vuelo NO se congela, por vieja que parezca: lastUse solo dice
	// cuándo llegó algo, no si sigue corriendo. Aplica igual a la primaria y a las
	// réplicas de scale-out; una réplica con sesiones activas mantiene su lastUse
	// fresco (ver route), así que solo se congelan las que de verdad quedaron
	// ociosas. Se recogen todas y se quitan después, para no mutar mientras se
	// recorre g.extra.
	for svc, e := range g.services {
		if e.inflight == 0 && time.Since(e.lastUse) > g.idle {
			victims = append(victims, victim{svc, e.machineID})
		}
	}
	for svc, es := range g.extra {
		for _, e := range es {
			if e.inflight == 0 && time.Since(e.lastUse) > g.idle {
				victims = append(victims, victim{svc, e.machineID})
			}
		}
	}
	for _, v := range victims {
		g.removeEntryLocked(v.service, v.id)
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
		log.Printf("%s: frozen due to inactivity", v.service)
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
//
// FAIRNESS (reparto justo, NO seguridad): quien pide sitio sacrifica PRIMERO lo
// SUYO. Se busca víctima entre las instancias del mismo tenant que pide, y solo
// si no hay ninguna se recurre a las de otro. Así la presión de un tenant no
// desaloja el trabajo de otro mientras aún le quede algo propio ocioso que ceder.
// No aísla nada —todo comparte daemon y bridge— pero evita el accidente de que un
// cliente activo eche a los demás de la memoria.
func (g *Gateway) evictLRU(ctx context.Context, salvo, tenant string) string {
	// pick elige la instancia ociosa más antigua, filtrando por tenant: con
	// mismo=true solo mira las del tenant que pide; con mismo=false, solo las de
	// los demás.
	pick := func(mismo bool) (string, string) {
		var elegido, id string
		var masAntiguo time.Time
		consid := func(svc string, e *entry) {
			if svc == salvo || e.inflight > 0 {
				return
			}
			if mismo != (e.tenant == tenant) {
				return
			}
			if elegido == "" || e.lastUse.Before(masAntiguo) {
				elegido, masAntiguo, id = svc, e.lastUse, e.machineID
			}
		}
		// Candidatas: la primaria y las réplicas de scale-out de cada servicio. Una
		// réplica ociosa es tan sacrificable como cualquier otra instancia.
		for svc, e := range g.services {
			consid(svc, e)
		}
		for svc, es := range g.extra {
			for _, e := range es {
				consid(svc, e)
			}
		}
		return elegido, id
	}

	g.mu.Lock()
	// Primero lo propio; si no hay, lo ajeno.
	elegido, id := pick(true)
	if elegido == "" {
		elegido, id = pick(false)
	}
	if elegido != "" {
		// Se saca del mapa (primaria o réplica, por machineID) ANTES de congelar:
		// si alguien pide ese servicio mientras tanto, que lo reconstruya en vez de
		// enrutar a una máquina que está a punto de dejar de existir.
		g.removeEntryLocked(elegido, id)
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
		log.Printf("could not freeze %s to make room: %v", elegido, err)
		return ""
	}
	return elegido
}
