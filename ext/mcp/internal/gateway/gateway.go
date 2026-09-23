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
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/http/pprof"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/guest"

	"github.com/juan52878911/kindling-mcp/internal/mcp"
	"github.com/juan52878911/kindling/pkg/panico"
	"github.com/juan52878911/kindling/pkg/scheduler"
)

// SessionHeader identifica la conversación MCP. El gateway la usa para enrutar
// SIEMPRE a la misma instancia: el estado de una sesión vive en el proceso del
// servidor MCP, así que mandar la segunda petición a otra microVM la rompería.
const SessionHeader = "Mcp-Session-Id"

// GuestPort es donde escucha el puente dentro de la microVM.
const GuestPort = api.GuestPort

// maxProxyBody acota lo que se guarda en memoria para poder reintentar. Coincide
// con el límite que ya aplica el puente al leer una petición.
const maxProxyBody = 8 << 20

// Gateway enruta llamadas MCP a microVMs. La planificación —despertar, congelar
// por inactividad, réplicas, precalentado, cuotas— es de pkg/scheduler; aquí
// vive lo que es MCP: la cabecera de sesión, el agregador _all, el catálogo, la
// memoria de uso, los servidores externos enlazados y la salud de cada servicio.
type Gateway struct {
	*scheduler.Scheduler

	PprofEnabled bool // expone /debug/pprof en Handler(); debe decidirlo el operador

	agg *aggregator // endpoint virtual que reúne a todos
	mem *memory     // memoria de uso; nil si está desactivada

	// Último estado de salud escrito por servicio, para no repetir la escritura
	// en cada petición. Ver anotarSalud.
	saludMu    sync.Mutex
	saludVista map[string]bool

	// Servidores MCP externos enlazados: no corren aquí, solo se enrutan.
	linkMu    sync.RWMutex
	linkCache []*mcp.Link
	linkAt    time.Time
}

func New(client *api.Client, idle time.Duration, ephemeral bool, prewarm int, memService string) *Gateway {
	g := &Gateway{Scheduler: scheduler.New(client, idle, ephemeral, prewarm)}
	g.agg = newAggregator(g, ephemeral)
	g.mem = newMemory(g, memService)

	// Lo que el planificador no sabe de MCP entra por sus ganchos.
	//
	// Una precalentada se entrega con la sesión MCP ya abierta: el initialize
	// se paga al calentarla, no cuando llega la petición.
	//
	// PrepareAddr (no Prepare): en macOS la IP del invitado no se alcanza desde
	// el host y solo la dirección con el reenvío sirve (ver docs/backend-vz.md
	// §3). PrepareAddr gana a Prepare en pkg/scheduler, así que basta con fijar
	// este.
	g.PrepareAddr = func(ctx context.Context, addr string) (string, error) {
		return mcpInit(ctx, "http://"+addr)
	}
	// Los servicios con estado no se precalientan: su instancia es persistente.
	g.Skip = mcp.Stateful
	// Una instancia que no contesta marca la salud del servicio.
	g.OnProxyError = g.anotarFallo
	// Las sesiones del agregador viven más que las instancias: son baratas.
	g.OnTick = func(context.Context) { g.agg.reap(g.Idle() * 4) }
	return g
}

// Handler expone las rutas del gateway.
//
// El token llega por parámetro en vez de vivir en el Gateway para que arrancar
// sin autenticación sea una decisión explícita de quien compone el servidor: el
// compilador obliga a escribir algo, aunque sea la cadena vacía.
func (g *Gateway) Handler(token string) http.Handler {
	// El registro va POR FUERA de la autenticación: los 401 son justo lo que
	// hay que poder ver cuando alguien sondea el puerto. AuthHandler resuelve
	// el token a un tenant (el único = "default") y lo cuelga del contexto
	// para que handleProxy pueda aplicar las cuotas.
	return logging(g.AuthHandler(g.routes(), token))
}

// routes construye el mux SIN autenticación ni registro.
//
// Separado de Handler para que Router (multi-host) pueda reutilizar las rutas
// de cada host tal cual —proxy, cuotas, sesiones pegajosas, todo lo que ya
// tenía un solo host— y envolver el token UNA sola vez él mismo, en vez de una
// vez por host.
func (g *Gateway) routes() http.Handler {
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

	return mux
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
	snaps, err := g.Client().Snapshots(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, s := range snaps {
		name := s.Name
		if svc := s.Service(); svc != "" {
			name = svc
		}
		st := g.Status(name)
		status := "cold"
		if st.Prewarmed > 0 {
			status = fmt.Sprintf("%d prewarmed instance(s)", st.Prewarmed)
		}
		if st.Warm {
			// st.Addr (host:puerto real) en vez de st.IP: en macOS la IP del
			// invitado es la misma para todas las instancias y no dice nada.
			status = fmt.Sprintf("warm at %s · %d session(s) · idle %s",
				st.Addr, st.Sessions, st.Idle.Round(time.Second))
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
		body, err := api.LeerCuerpo(r.Body, maxProxyBody)
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

	// Las rutas de control del agente (/resync, /volume/*, /exec, /files, /dns)
	// y el /reset del puente son del HOST, no de los clientes del servicio. El
	// puente atiende "/" entero y el proxy reenvía cualquier ruta, así que sin
	// este corte un cliente con token podía mover el reloj de la microVM,
	// desmontarle los volúmenes o cerrar las sesiones de los demás.
	if p := path.Clean(r.URL.Path); guest.IsControlPath(p) || p == "/reset" {
		http.NotFound(w, r)
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
	g.Observe(service)

	// Cuota de peticiones en vuelo del tenant. Es reparto justo, NO seguridad
	// (ver quota.go): un solo cliente en bucle no debe acaparar todas las
	// conexiones y dejar a los demás esperando. El 429 envuelve TODA la petición
	// —tanto el camino pegajoso como el que despierta instancia— con un único
	// begin/end, así que basta contabilizar aquí una vez.
	tnt := scheduler.TenantFrom(r.Context())
	if !g.TenantBegin(tnt) {
		http.Error(w, fmt.Sprintf(
			"429: tenant %q has reached its quota of %d in-flight requests.\n"+
				"This is a fair-share limit, not a security limit: retry once the earlier ones finish.",
			tnt.Name(), tnt.MaxInflight()), http.StatusTooManyRequests)
		return
	}
	defer g.TenantEnd(tnt)

	// Sesión ya conocida: directo a su instancia.
	//
	// El id que trae el cliente es SIEMPRE uno acuñado por el gateway, nunca el
	// del invitado: el del invitado sale de un proceso que tratamos como hostil,
	// y réplicas restauradas del mismo snapshot llegaron a dar ids idénticos.
	// Con el id del invitado como clave del mapa de rutas, el segundo cliente
	// reapuntaba en silencio la sesión del primero a su microVM. La ruta guarda
	// el del invitado aparte y aquí se traduce en los dos sentidos.
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
	if ext := r.Header.Get(SessionHeader); ext != "" {
		rt := g.Route(ext)
		if rt == nil || rt.Service() != service {
			// Id desconocido, caducado, inventado o de otro servicio: NO se
			// reenvía al invitado —que podría tener una sesión con ese id, de
			// otro cliente—. 404 es lo que pide el transporte Streamable HTTP
			// para una sesión que no existe: el cliente rehace el initialize.
			http.Error(w, "unknown or expired MCP session; start a new one with initialize",
				http.StatusNotFound)
			return
		}
		// La instancia de la sesión puede ser la primaria O una réplica de
		// scale-out: se busca por machineID entre todas, no solo la primaria
		// (mirar solo g.services rompía las sesiones enrutadas a una réplica).
		e := g.Instance(rt.Service(), rt.MachineID())

		// Dos formas de que la instancia haya muerto bajo la sesión: que el
		// GATEWAY la retirara (ya no aparece por machineID) o que el DAEMON la
		// congelara por TTL (aún figura, pero el invitado no responde). Lo
		// segundo solo se ve comprobando vida.
		if e == nil || !scheduler.AliveAddr(rt.Addr(GuestPort)) {
			// Se invalida la instancia congelada (si aún figura) para que
			// ensure la reconstruya en vez de devolverla tal cual, y se
			// reconstruye la primaria del servicio.
			g.DropInstance(rt.Service(), rt.MachineID())
			var err error
			e, err = g.Ensure(r.Context(), rt.Service())
			if err != nil {
				g.Forget(ext)
				if errors.Is(err, scheduler.ErrTenantInstances) {
					http.Error(w, fmt.Sprintf("could not recover session for %q: %v", rt.Service(), err),
						http.StatusTooManyRequests)
					return
				}
				http.Error(w, fmt.Sprintf("could not recover session for %q: %v", rt.Service(), err),
					http.StatusBadGateway)
				return
			}
			if e.MachineID() != rt.MachineID() {
				// Máquina distinta: su puente no conoce esta sesión. Se olvida y
				// el cliente rehace el handshake contra la instancia nueva.
				g.Forget(ext)
				http.Error(w, "the MCP session was lost with its instance; start a new one with initialize",
					http.StatusNotFound)
				return
			}
			// Mismo VMM descongelado: la sesión del puente sigue viva, solo hay
			// que reapuntar al proxy nuevo.
			g.Rebind(ext, e)
			if rt = g.Route(ext); rt == nil {
				http.Error(w, "unknown or expired MCP session; start a new one with initialize",
					http.StatusNotFound)
				return
			}
		}

		g.Begin(e)
		defer g.End(e)
		r.Header.Set(SessionHeader, guestSIDOf(rt, ext))
		rt.ServeHTTP(&sidWriter{ResponseWriter: w, ext: ext}, r)
		// DELETE cierra la sesión: se olvida la ruta para no acumularlas.
		if r.Method == http.MethodDelete {
			g.Forget(ext)
		}
		return
	}

	// Sesión NUEVA (initialize): se coloca en una instancia con hueco, escalando a
	// una réplica si todas están llenas. Es lo que permite el uso en paralelo.
	g.serveNewSession(w, r, service, tnt)
}

// guestSIDOf es el id que el INVITADO dio a la sesión ext. Una ruta fijada con
// Bind (sin id del invitado aparte) usa la clave tal cual.
func guestSIDOf(rt *scheduler.Route, ext string) string {
	if g := rt.GuestSID(); g != "" {
		return g
	}
	return ext
}

// sidWriter traduce la cabecera de sesión de la respuesta del invitado al id
// externo antes de que salga: el cliente no debe ver nunca el id del invitado,
// ni adoptar otro que el invitado quiera colarle. Con ext vacío la quita.
type sidWriter struct {
	http.ResponseWriter
	ext   string
	hecho bool
}

func (s *sidWriter) reescribir() {
	if s.hecho {
		return
	}
	s.hecho = true
	h := s.ResponseWriter.Header()
	if len(h.Values(SessionHeader)) == 0 {
		return
	}
	if s.ext == "" {
		h.Del(SessionHeader)
		return
	}
	h.Set(SessionHeader, s.ext)
}

func (s *sidWriter) WriteHeader(code int) {
	// Las 1xx no son la respuesta final: las cabeceras aún pueden cambiar.
	if code >= 200 {
		s.reescribir()
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *sidWriter) Write(b []byte) (int, error) {
	s.reescribir()
	return s.ResponseWriter.Write(b)
}

// Flush hace falta para el streaming (SSE) de una sesión: sin reenviarlo, las
// respuestas se quedarían en el buffer hasta cerrar.
func (s *sidWriter) Flush() {
	s.reescribir()
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap deja a http.ResponseController (que usa ReverseProxy) llegar al
// ResponseWriter de verdad.
func (s *sidWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

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
func (g *Gateway) serveNewSession(w http.ResponseWriter, r *http.Request, service string, tnt *scheduler.Tenant) {
	// Sin cuerpo reenviable no hay reintento posible: se sirve directo por la
	// primaria. handleProxy prepara GetBody para los POST, así que esto solo pasa
	// con métodos sin cuerpo, que no crean sesión y por tanto nunca dan el 400 de
	// tope.
	if r.GetBody == nil {
		e, err := g.Ensure(r.Context(), service)
		if err != nil {
			g.newSessionError(w, r, service, err)
			return
		}
		g.Begin(e)
		defer g.End(e)
		// El codigo se mira DESPUES de servir: anotar el exito por haber
		// conseguido la instancia daba por sano un servicio que no contestaba.
		// Sin sesión fijada no hay id externo que dar: si el invitado pone uno,
		// se quita, para que el cliente no acabe usando el del invitado.
		cw := &codigoVisto{ResponseWriter: &sidWriter{ResponseWriter: w}, code: http.StatusOK}
		e.Proxy().ServeHTTP(cw, r)
		if cw.code < 500 {
			g.anotarExito(service)
		}
		return
	}

	for try := 0; try < maxScaleOut; try++ {
		var e *scheduler.Instance
		var err error
		if try == 0 {
			e, err = g.PickInstance(r.Context(), service, tnt)
		} else {
			// El intento anterior chocó con el tope de una instancia: se fuerza una
			// réplica nueva para esta sesión.
			e, err = g.ScaleOut(r.Context(), service, tnt)
		}
		if err != nil {
			g.newSessionError(w, r, service, err)
			return
		}
		if b, berr := r.GetBody(); berr == nil {
			r.Body = b
		}
		rec := httptest.NewRecorder()
		g.Begin(e)
		e.Proxy().ServeHTTP(rec, r)
		g.End(e)

		// ¿El puente rechazó por tope de sesiones? Esa instancia está llena: se crea
		// otra y se reintenta. Cualquier otra respuesta (incluido otro 400) se
		// entrega tal cual al cliente.
		if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "session limit") {
			continue
		}

		// Sano si SIRVIO, no si se consiguio instancia. Un 5xx aqui ya lo anoto
		// el ErrorHandler como fallo; anotar exito tambien lo borraria.
		if rec.Code < 500 {
			g.anotarExito(service)
		}

		// Fijar la ruta de la sesión a ESTA instancia (primaria o réplica) ANTES
		// de contestar —un cliente rápido manda la siguiente petición en cuanto
		// lee la respuesta— y entregar la respuesta bufereada.
		//
		// El cliente recibe un id ACUÑADO aquí, no el del invitado: ver
		// handleProxy. Dos instancias que den el mismo id acaban en dos
		// sesiones distintas, cada una con su microVM.
		ext := ""
		if guestSID := rec.Header().Get(SessionHeader); guestSID != "" {
			var berr error
			for i := 0; i < 3; i++ {
				ext = scheduler.NewSessionKey()
				if berr = g.BindGuest(ext, guestSID, service, e); berr == nil {
					break
				}
			}
			if berr != nil {
				http.Error(w, fmt.Sprintf("could not register the session for %q: %v", service, berr),
					http.StatusInternalServerError)
				return
			}
		}
		for k, vs := range rec.Header() {
			if k == SessionHeader {
				continue
			}
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		if ext != "" {
			w.Header().Set(SessionHeader, ext)
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return
	}
	http.Error(w, fmt.Sprintf("could not place session for %q: all replicas full or no room on host", service),
		http.StatusServiceUnavailable)
}

func (g *Gateway) newSessionError(w http.ResponseWriter, r *http.Request, service string, err error) {
	// Sin snapshot puede ser un enlace recién creado que la caché aún no ve.
	if strings.Contains(err.Error(), "no snapshot for service") {
		if l := g.linkRecien(r.Context(), service); l != nil {
			// El cuerpo pudo leerse en un intento anterior: se rebobina.
			if r.GetBody != nil {
				if b, gerr := r.GetBody(); gerr == nil {
					r.Body = b
				}
			}
			g.handleLinkProxy(w, r, l)
			return
		}
	}
	// La cuota de instancias del tenant es un 429 (reparto justo), no un 502: el
	// servicio no falla, es que este tenant ya tiene todas las suyas.
	if errors.Is(err, scheduler.ErrTenantInstances) {
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
	// El recuerdo en memoria se actualiza SIEMPRE, haya daemon o no: es lo que
	// evita una escritura por peticion, y no depende de poder persistir.
	if !g.saludCambio(service, sano) {
		return
	}
	// Lo que si depende del daemon es escribirlo en el meta. Sin cliente no hay
	// donde, y la goroutine de abajo desreferenciaria un nulo — que, por ser una
	// goroutine, se llevaria el PROCESO entero y no solo esta anotacion.
	if g.Client() == nil {
		return
	}
	go func() {
		panico.Contener("gateway.anotarSalud", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := mcp.SetHealth(ctx, g.Client(), service, sano, causa); err != nil {
				log.Printf("%s: couldn't record its health in the snapshot: %v", service, err)
			}
		})
	}()
}

// handleLinkProxy reenvía la petición HTTP al servidor MCP externo.
//
// El enlace expone la URL completa de su servidor (p. ej. http://host:8080/mcp).
// El cliente, en cambio, pidió `/mcp/<servicio>/...` y ya le quitamos el prefijo
// en el llamador, así que r.URL.Path empieza por `/mcp`. Para que el proxy no
// concatene dos veces la ruta, se elimina el sufijo `/mcp` de la URL del enlace
// antes de construir el destino.
func (g *Gateway) handleLinkProxy(w http.ResponseWriter, r *http.Request, l *mcp.Link) {
	base := strings.TrimSuffix(l.URL, "/mcp")
	target, err := url.Parse(base)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid link URL: %v", err), http.StatusInternalServerError)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	// El destino es una URL de TERCEROS, puesta con `kling mcp link`. Mandarle el
	// token del gateway seria filtrar la credencial de todo el sistema a un host
	// que no controlamos.
	scheduler.WithoutGatewayCredential(proxy)
	proxy.ServeHTTP(w, r)
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

// codigoVisto recuerda el codigo con el que se respondio, para poder decidir la
// salud por el RESULTADO. httptest.NewRecorder no vale en el camino sin cuerpo
// reenviable: ahi se sirve directo al cliente, sin bufferear.
type codigoVisto struct {
	http.ResponseWriter
	code int
}

func (c *codigoVisto) WriteHeader(code int) {
	c.code = code
	c.ResponseWriter.WriteHeader(code)
}

// Flush hace falta para el streaming (SSE): sin reenviarlo, envolver el
// ResponseWriter dejaria las respuestas en el buffer hasta cerrar.
func (c *codigoVisto) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
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
func (g *Gateway) links(ctx context.Context) []*mcp.Link { return g.linksCon(ctx, false) }

// linksCon es links; forzar relee aunque la caché esté fresca, salvo que se
// haya leído hace menos de 100 ms (para que una ráfaga de peticiones a un
// servicio inexistente no se convierta en una ráfaga contra el daemon). No más:
// con un segundo, enlazar un servicio y usarlo acto seguido seguía fallando.
func (g *Gateway) linksCon(ctx context.Context, forzar bool) []*mcp.Link {
	g.linkMu.RLock()
	edad := time.Since(g.linkAt)
	if edad < 30*time.Second && (!forzar || edad < 100*time.Millisecond) {
		out := g.linkCache
		g.linkMu.RUnlock()
		return out
	}
	g.linkMu.RUnlock()

	ls, err := mcp.Links(ctx, g.Client())
	if err != nil {
		return nil
	}
	g.linkMu.Lock()
	g.linkCache, g.linkAt = ls, time.Now()
	g.linkMu.Unlock()
	return ls
}

// linkFor busca el enlace que sirve a un servicio, o nil si lo sirve una microVM.
func (g *Gateway) linkFor(ctx context.Context, service string) *mcp.Link {
	return buscarLink(g.links(ctx), service)
}

// linkRecien relee los enlaces aunque la caché esté fresca y busca ahí el
// servicio. Se usa SOLO cuando el servicio no tiene snapshot, justo antes de
// contestar que no existe: un `kling mcp link` recién hecho no existía para el
// gateway durante los 30 s de caché, y la primera petición acababa en "no
// snapshot for service" (lo encontró el e2e de la extensión). Llamarlo en cada
// fallo de caché costaría una lectura del store por cada petición a un
// servicio en microVM, que nunca está entre los enlaces.
func (g *Gateway) linkRecien(ctx context.Context, service string) *mcp.Link {
	return buscarLink(g.linksCon(ctx, true), service)
}

func buscarLink(ls []*mcp.Link, service string) *mcp.Link {
	for _, l := range ls {
		if l.Service() == service || l.Name == service {
			return l
		}
	}
	return nil
}
