// Package gateway enruta llamadas MCP a microVMs bajo demanda.
//
// Es un proceso APARTE del daemon, y a propósito: el daemon nunca escucha en la
// red porque controlarlo equivale a root en su host. El gateway sí escucha, pero
// su superficie es mucho más estrecha — solo sabe despertar instancias de
// snapshots ya existentes y hacer de proxy.
//
//	cliente MCP ──HTTP──> gateway ──socket unix──> daemon ──> microVM
//	                          └────────HTTP proxy───────────────┘
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/panico"
)

// GuestPort es donde se espera que escuche el servidor MCP dentro de la microVM.
const GuestPort = api.GuestPort

// preguntárselo al daemon. El vigilante del daemon detecta las muertes en 10 s,
// así que comprobarlo más a menudo desde aquí no aporta nada y sí cuesta.
const livenessTTL = 15 * time.Second

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
func alive(ip string, port int) bool { return aliveAddr(net.JoinHostPort(ip, strconv.Itoa(port))) }

// aliveAddr es alive sobre una dirección host:puerto ya resuelta. Es la forma
// que vale en los dos sistemas: en macOS el invitado no se alcanza por su IP
// sino por el reenvío que da api.Machine.Addr.
func aliveAddr(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 400*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// waitReady sondea el puerto del invitado hasta que acepta conexiones.
func waitReady(ctx context.Context, ip string, port int, timeout time.Duration) error {
	return waitReadyAddr(ctx, net.JoinHostPort(ip, strconv.Itoa(port)), timeout)
}

// waitReadyAddr es waitReady sobre una dirección host:puerto ya resuelta.
func waitReadyAddr(ctx context.Context, addr string, timeout time.Duration) error {
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

// esperarListo espera a que el agente del invitado escuche.
//
// En macOS (la máquina trae reenvíos) no vale sondear la dirección: el puerto
// de loopback lo abre kling-vz y acepta siempre, escuche el invitado o no. Se
// pregunta al daemon con probe_only, que mira dentro del invitado. En Linux,
// el dial directo de siempre, que es submilisegundo.
func (g *Scheduler) esperarListo(ctx context.Context, mc *api.Machine, timeout time.Duration) error {
	if len(mc.Forwards) > 0 && g.client != nil {
		_, err := g.client.Guest(ctx, mc.ID, api.GuestRequest{
			Port: GuestPort, ProbeOnly: true, WaitMS: int(timeout / time.Millisecond),
		})
		return err
	}
	return waitReadyAddr(ctx, mc.Addr(GuestPort), timeout)
}

// Scheduler mantiene instancias calientes por servicio: las despierta al llegar
// trabajo, las congela cuando se quedan ociosas, añade réplicas cuando una no da
// abasto, desaloja por memoria y precalienta las más usadas.
//
// No sabe qué protocolo habla el invitado. El gateway MCP lo usa con la cabecera
// Mcp-Session-Id como clave de afinidad; un sandbox usaría su identificador. Lo
// específico de cada uno entra por los ganchos (Prepare, Skip, OnProxyError,
// OnTick).
type Scheduler struct {
	client    *api.Client
	idle      time.Duration
	Ephemeral bool
	// KeepWarm mantiene caliente la primaria de los N servicios más populares en
	// modo PERSISTENTE, para que la primera sesión no pague el arranque en frío
	// (~16 s bajo KVM anidado en Mac; ~3 s en Linux nativo). 0 = desactivado (por
	// defecto). Lo decide el operador: en un host con la RAM
	// justa (la caja x86 de pruebas) mantener instancias que nadie usa provoca
	// desalojos, así que es una palanca para hosts donde el cuello es el ARRANQUE y
	// no la memoria. A diferencia del prewarm (fondo efímero), esto puebla
	// g.services normales: la siguiente petición reutiliza la instancia caliente, o
	// —si el segador la congeló por ociosa— paga solo un thaw (~25 ms).
	KeepWarm int
	// MaxInflight es cuántas peticiones en vuelo aguanta una instancia antes de
	// que la siguiente sesión provoque una réplica nueva, aunque aún le quepan
	// sesiones. 0 = solo cuenta sesiones, como siempre.
	MaxInflight int
	// MaxReplicas acota cuántas instancias puede tener un servicio al escalar
	// por carga. 0 = sin tope (el límite lo pone la memoria del host).
	MaxReplicas int
	// freezeFn sustituye la llamada al daemon en los tests. En producción es nil
	// y se usa el cliente: el desalojo por falta de memoria no se puede ejercitar
	// de otro modo sin levantar un daemon con KVM.
	freezeFn func(id string) error
	mu       sync.Mutex
	services map[string]*entry        // servicio -> instancia "por defecto" (primaria)
	extra    map[string][]*entry      // servicio -> RÉPLICAS de scale-out (además de la primaria)
	routes   map[string]*sessionRoute // clave de sesión -> instancia fija (afinidad)
	pool     *pool                    // instancias pre-calentadas por servicio
	ensureMu sync.Map                 // servicio -> *sync.Mutex; ver ensure()
	pop      *popularity              // popularidad por servicio; guía el prewarm

	// Cuotas por tenant (token con nombre). Reparto justo, NO seguridad: ver
	// quota.go. extraTenants son los tokens con nombre; tenantInflight lleva la
	// cuenta de peticiones en vuelo por tenant, bajo su propio candado para no
	// pelear con g.mu en el camino caliente.
	extraTenants   []TenantLimit
	tenantMu       sync.Mutex
	tenantInflight map[string]int

	// GANCHOS. Lo que depende del protocolo del invitado entra por aquí; todos
	// son opcionales.

	// Prepare deja lista una instancia recién restaurada para el pool (el
	// gateway MCP hace el initialize). Devuelve un testigo opaco que acompaña a
	// la instancia (la sesión MCP abierta). nil = basta con que escuche.
	Prepare func(ctx context.Context, ip string) (token string, err error)

	// PrepareAddr es Prepare recibiendo la dirección host:puerto del agente
	// (api.Machine.Addr(GuestPort)) en vez de la IP. Si está, gana a Prepare:
	// en macOS la IP del invitado no se alcanza desde el host y solo la
	// dirección sirve. Prepare se conserva para no romper a quien ya lo usa.
	PrepareAddr func(ctx context.Context, addr string) (token string, err error)

	// Skip excluye un snapshot del precalentado y del keepwarm (el gateway MCP
	// excluye los servicios con estado).
	Skip func(*api.Snapshot) bool

	// OnProxyError se llama cuando una instancia no contesta a una petición
	// reenviada (el gateway MCP anota la salud del servicio).
	OnProxyError func(service string, err error)

	// OnTick corre en cada vuelta del segador, dentro de la misma protección
	// contra pánicos (el gateway MCP poda sus sesiones de agregador).
	OnTick func(ctx context.Context)
}

type entry struct {
	machineID string
	ip        string
	// fwd son los reenvíos de puertos de la máquina (api.Machine.Forwards):
	// vacío en Linux. Se guardan para resolver direcciones con Addr sin volver
	// a preguntar al daemon.
	fwd     map[string]string
	lastUse time.Time
	proxy   *httputil.ReverseProxy

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
func (g *Scheduler) begin(e *entry) {
	g.mu.Lock()
	e.inflight++
	e.lastUse = time.Now()
	g.mu.Unlock()
}

func (g *Scheduler) end(e *entry) {
	g.mu.Lock()
	if e.inflight > 0 {
		e.inflight--
	}
	e.lastUse = time.Now()
	g.mu.Unlock()
}

// sessionRoute recuerda a qué instancia pertenece cada sesión MCP.
type sessionRoute struct {
	service string
	// guestSID es el id de sesión que dio el INVITADO, cuando quien enruta
	// acuña su propia clave (BindGuest). Vacío si la clave es la del invitado.
	//
	// Se separan porque el id del invitado no es de fiar: sale de un proceso
	// que este proyecto trata como hostil, y dos réplicas del mismo snapshot
	// llegaron a dar ids idénticos (CSPRNG copiado en la restauración). Usarlo
	// como clave de un mapa global dejaba que una sesión pisara a otra.
	guestSID  string
	machineID string
	ip        string
	fwd       map[string]string
	proxy     *httputil.ReverseProxy
	lastUse   time.Time
}

// pickInstance devuelve una instancia del servicio con hueco de sesión. Despierta
// la primaria si hace falta, y si todas las instancias (primaria + réplicas) están
// al tope, crea una réplica nueva.
// elegirInstancia decide a qué instancia va una sesión nueva. Devuelve la
// elegida si hay una con hueco de sesión y sin saturar de trabajo; si todas las
// que tienen hueco están saturadas, devuelve nil y la menos cargada de ellas
// (libre), por si no se puede escalar. Pura, para poder probarla sin daemon.
func elegirInstancia(entries []*entry, sesiones func(string) int, maxInflight int) (elegida, libre *entry) {
	for _, e := range entries {
		if sesiones(e.machineID) >= e.maxSessions {
			continue
		}
		if maxInflight <= 0 || e.inflight < maxInflight {
			return e, nil
		}
		if libre == nil || e.inflight < libre.inflight {
			libre = e
		}
	}
	return nil, libre
}

func (g *Scheduler) pickInstance(ctx context.Context, service string, tnt *tenant) (*entry, error) {
	if _, err := g.ensure(ctx, service); err != nil {
		return nil, err
	}
	g.mu.Lock()
	elegida, libre := elegirInstancia(g.entriesLocked(service), g.sessionCountLocked, g.MaxInflight)
	replicas := len(g.entriesLocked(service))
	g.mu.Unlock()
	if elegida != nil {
		return elegida, nil
	}

	// Todas las que tienen hueco están saturadas de trabajo. Contar solo
	// sesiones dejaba a una réplica con una sesión y veinte llamadas en vuelo
	// sirviéndolo todo mientras sobraban recursos para otra. Se escala, con
	// tope, y si no se puede se usa la menos cargada: una sesión atendida lenta
	// es mejor que una rechazada.
	if libre != nil {
		if g.MaxReplicas > 0 && replicas >= g.MaxReplicas {
			return libre, nil
		}
		e, err := g.scaleOut(ctx, service, tnt)
		if err != nil {
			return libre, nil
		}
		return e, nil
	}
	return g.scaleOut(ctx, service, tnt)
}

// sinCredencialDelGateway quita la cabecera Authorization antes de reenviar.
//
// `Authorization` NO es hop-by-hop, asi que httputil.ReverseProxy la propaga
// verbatim. Sin esto, el token que autentica a los clientes del gateway llega
// entero a cada servidor MCP invitado —que este codigo declara HOSTILES por
// diseno, ver internal/machine/mmds.go— y a cada URL externa enlazada.
//
// Con ese token, un invitado comprometido llama al gateway como cliente
// legitimo: despertar un snapshot ES ejecutar codigo, y desde ahi puede invocar
// cualquier herramienta de cualquier servicio y cruzar tenants. Las cuotas no
// son una frontera de seguridad y no lo impedirian.
//
// Lo que el destino necesite para autenticarse es SUYO y se anade aparte: los
// enlaces (api.Link) no llevan credencial, asi que aqui no hay nada que
// reponer.
// proxyInvitado arma el proxy hacia una microVM, sin la credencial del gateway.
func proxyInvitado(target *url.URL) *httputil.ReverseProxy {
	p := httputil.NewSingleHostReverseProxy(target)
	sinCredencialDelGateway(p)
	return p
}

func sinCredencialDelGateway(p *httputil.ReverseProxy) {
	dir := p.Director
	p.Director = func(r *http.Request) {
		if dir != nil {
			dir(r)
		}
		r.Header.Del("Authorization")
	}
}

func (g *Scheduler) route(sid string) *sessionRoute {
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

// errSessionTaken: la clave ya está fijada a OTRA instancia viva.
var errSessionTaken = errors.New("session key already bound to another instance")

// bind fija la sesión sid a e. Si sid ya estaba fijada a otra instancia NO la
// pisa y devuelve errSessionTaken: antes se sobrescribía a ciegas, y dos
// clientes que recibían el mismo id del invitado acababan con la sesión del
// primero apuntando a la microVM del segundo. Volver a fijarla a la MISMA
// instancia sí vale (actualiza guestSID).
func (g *Scheduler) bind(sid, guestSID, service string, e *entry) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if prev, ok := g.routes[sid]; ok && (prev.service != service || prev.machineID != e.machineID) {
		log.Printf("%s: session %s is already bound to another instance (%s); refusing to re-point it",
			service, short(sid), short(prev.machineID))
		return errSessionTaken
	}
	g.routes[sid] = &sessionRoute{
		service: service, guestSID: guestSID, machineID: e.machineID, ip: e.ip, fwd: e.fwd,
		proxy: e.proxy, lastUse: time.Now(),
	}
	log.Printf("%s: session %s bound to %s", service, short(sid), e.Addr(GuestPort))
	return nil
}

// rebind reapunta una sesión a la instancia actual de su servicio, conservando
// la sesión: se usa cuando el MISMO VMM se descongeló y solo cambió el proxy.
func (g *Scheduler) rebind(sid string, e *entry) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if rt, ok := g.routes[sid]; ok {
		rt.machineID, rt.ip, rt.fwd, rt.proxy = e.machineID, e.ip, e.fwd, e.proxy
		rt.lastUse = time.Now()
	}
}

func (g *Scheduler) forget(sid string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.routes, sid)
}

// ensure devuelve una instancia viva del servicio, despertándola si hace falta.
//
// Tres casos, de más barato a más caro:
//  1. ya hay una caliente          -> 0 ms
//  2. hay una congelada            -> ~30 ms de thaw
//  3. no hay ninguna               -> se instancia del snapshot dorado
func (g *Scheduler) ensure(ctx context.Context, service string) (*entry, error) {
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
func (g *Scheduler) buildEntry(ctx context.Context, service string, tnt *tenant, fresh bool) (*entry, error) {
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
	for api.IsInsufficientMemory(err) || api.IsMachineLimit(err) {
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
		//
		// El tope de máquinas del daemon no se arregla congelando: una máquina
		// congelada sigue contando. Lo único que baja el contador es destruir, y
		// lo que se puede destruir sin perder trabajo de nadie es el fondo de
		// precalentadas.
		if api.IsMachineLimit(err) {
			if g.pool == nil || !g.pool.evictOne(ctx) {
				break
			}
			log.Printf("%s: at the daemon's machine limit; dropped a prewarmed instance", service)
			mc, err = g.acquire(ctx, service, fresh)
			continue
		}
		victima := g.evictLRU(ctx, service, tnt.name)
		if victima == "" {
			// No queda nada ocioso que sacrificar: ahora sí hay que rendirse, y
			// el error de falta de memoria explica por qué.
			break
		}
		if victima == evictedPool {
			log.Printf("%s: didn't fit; dropped a prewarmed instance to make room", service)
		} else {
			log.Printf("%s: didn't fit; froze %s to make room", service, victima)
		}
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
	if err := g.esperarListo(ctx, mc, readyTimeout); err != nil {
		return nil, fmt.Errorf("tool did not start listening: %w", err)
	}
	// Cuánto tardó el 8080 en aceptar es la métrica que discrimina el cuello de
	// botella del arranque (ver docs de rendimiento en Mac): un thaw acepta casi al
	// instante, un arranque en frío bajo KVM anidado tarda segundos. Se registra solo
	// cuando es notable, para no ensuciar el log con los thaws de milisegundos.
	if d := time.Since(wr0); d > 500*time.Millisecond {
		log.Printf("%s: waitReady %v (fresh=%v)", service, d.Round(time.Millisecond), fresh)
	}

	target, _ := url.Parse("http://" + mc.Addr(GuestPort))
	e := &entry{
		machineID:   mc.ID,
		ip:          mc.IP,
		fwd:         mc.Forwards,
		lastUse:     time.Now(),
		checkedAt:   time.Now(),
		tenant:      tnt.name, // quien la despertó es su dueño para cuota y fairness
		maxSessions: gwMaxSessions(mc.MemMiB),
		proxy:       proxyInvitado(target),
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
		// Se sondea SIEMPRE que el fallo sea de dial, no solo cuando toca
		// reintentar: la respuesta distingue dos situaciones muy distintas —el
		// invitado esta ahi y no acepto esa conexion, o el invitado YA NO EXISTE.
		vivo := esDial && waitReadyAddr(r.Context(), e.Addr(GuestPort), 3*time.Second) == nil

		if vivo && !retried(r) && r.GetBody != nil {
			body, berr := r.GetBody()
			if berr == nil {
				log.Printf("proxy %s: %v (one retry)", service, err)
				r2 := r.Clone(markRetried(r.Context()))
				r2.Body = body
				e.proxy.ServeHTTP(w, r2)
				return
			}
		}

		// No se pudo conectar Y el invitado tampoco responde ahora: esta
		// instancia ya no esta. Pasa de verdad — el recolector de disco del
		// daemon retira instancias dormidas para hacer sitio, y el gateway se
		// quedaba con la entrada apuntando a una IP muerta PARA SIEMPRE: todas
		// las peticiones siguientes daban 502 sin que nada lo arreglara. Se
		// olvida aqui para que la proxima peticion levante una nueva.
		if esDial && !vivo {
			log.Printf("proxy %s: its instance is gone; forgetting it so the next call creates another", service)
			g.olvidarInstancia(service, e.machineID)
		}
		// Y se DEGRADA la salud. Antes el exito se anotaba al conseguir la
		// instancia, no al servir: un servicio caido devolvia 502 una y otra vez
		// mientras cada intento REAFIRMABA que estaba sano.
		if g.OnProxyError != nil {
			g.OnProxyError(service, err)
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
func (g *Scheduler) entriesLocked(service string) []*entry {
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
func (g *Scheduler) entryByMachineLocked(service, machineID string) *entry {
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
func (g *Scheduler) sessionCountLocked(machineID string) int {
	n := 0
	for _, rt := range g.routes {
		if rt.machineID == machineID {
			n++
		}
	}
	return n
}

// olvidarInstancia retira una instancia que ya no existe, tomando el cerrojo.
// Es removeEntryLocked para quien no lo tiene.
func (g *Scheduler) olvidarInstancia(service, machineID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.removeEntryLocked(service, machineID)
}

// removeEntryLocked quita una instancia del servicio, sea la primaria o una
// réplica. Se llama con g.mu tomado.
func (g *Scheduler) removeEntryLocked(service, machineID string) {
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
func (g *Scheduler) scaleOut(ctx context.Context, service string, tnt *tenant) (*entry, error) {
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
func (g *Scheduler) acquire(ctx context.Context, service string, fresh bool) (*api.Machine, error) {
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
		if match(m) && m.State == api.StateRunning && m.Reachable() {
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
func (g *Scheduler) runFresh(ctx context.Context, service string) (*api.Machine, error) {
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

func (g *Scheduler) snapshotFor(ctx context.Context, service string) (*api.Snapshot, error) {
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

func (g *Scheduler) alive(ctx context.Context, id string) bool {
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
func (g *Scheduler) Reap(ctx context.Context) {
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
				if g.OnTick != nil {
					g.OnTick(ctx)
				}
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
func (g *Scheduler) PrewarmAll(ctx context.Context) {
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
		if g.Skip != nil && g.Skip(s) {
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
func (g *Scheduler) KeepWarmAll(ctx context.Context) {
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
		if g.Skip != nil && g.Skip(s) {
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
func (g *Scheduler) prewarmBudget(ctx context.Context) int {
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
func (g *Scheduler) Drain(ctx context.Context) {
	g.pop.fold() // deja el historial de popularidad en disco antes de irse
	g.pool.drain(ctx)
}

func (g *Scheduler) reapOnce(ctx context.Context) {
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

// ensureLock devuelve el candado de aprovisionamiento de un servicio.
func (g *Scheduler) ensureLock(service string) *sync.Mutex {
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
// evictedPool es lo que devuelve evictLRU cuando lo que liberó fue una instancia
// del fondo de precalentadas y no un servicio. No es un nombre de servicio, y
// por eso no puede confundirse con uno: los nombres válidos no llevan espacios.
const evictedPool = "(warm pool)"

func (g *Scheduler) evictLRU(ctx context.Context, salvo, tenant string) string {
	// pick elige la instancia ociosa más antigua, filtrando por tenant: con
	// mismo=true solo mira las del tenant que pide; con mismo=false, solo las de
	// los demás.
	// descartadas son las que ya se probaron y no se pudieron congelar. Sin
	// esto, evictLRU se rendia con la PRIMERA victima ocupada y devolvia un 507
	// al cliente habiendo RAM perfectamente liberable en otra instancia.
	descartadas := map[string]bool{}

	pick := func(mismo bool) (string, string) {
		var elegido, id string
		var masAntiguo time.Time
		consid := func(svc string, e *entry) {
			if svc == salvo || e.inflight > 0 || descartadas[e.machineID] {
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

	freeze := func(id string) error { _, err := g.client.Freeze(ctx, id); return err }
	if g.freezeFn != nil {
		freeze = g.freezeFn
	}

	// Hasta tres victimas. Antes se probaba UNA: si estaba ocupada en su propio
	// ensure, evictLRU devolvia "" y el llamador respondia 507 aunque hubiera
	// otras instancias ociosas de sobra.
	const intentos = 3
	for i := 0; i < intentos; i++ {
		g.mu.Lock()
		// Primero lo propio; si no hay, lo ajeno.
		elegido, id := pick(true)
		if elegido == "" {
			elegido, id = pick(false)
		}
		var victima *entry
		if elegido != "" {
			victima = g.entryByMachineLocked(elegido, id)
			// Se saca del mapa (primaria o réplica, por machineID) ANTES de
			// congelar: si alguien pide ese servicio mientras tanto, que lo
			// reconstruya en vez de enrutar a una máquina que está a punto de
			// dejar de existir.
			g.removeEntryLocked(elegido, id)
		}
		g.mu.Unlock()

		if elegido == "" {
			return "" // ya no queda ninguna candidata
		}

		// devolver repone la instancia. Se saco para que nadie enrutara a una
		// maquina a punto de congelarse; si al final NO se congela, dejarla
		// fuera es peor que no haberla tocado: el gateway cree que no existe,
		// y el siguiente ensure la adopta desde List() cuando ya no toca.
		devolver := func() {
			if victima == nil {
				return
			}
			g.mu.Lock()
			g.reponerEntryLocked(elegido, victima)
			g.mu.Unlock()
		}
		descartadas[id] = true

		// El candado del servicio víctima, para no congelar debajo de un ensure
		// concurrente: sin esto, ese ensure no la encuentra en el mapa, hace
		// List(), la ve todavía running (el freeze tarda ~2 s), la adopta como
		// "ya en marcha" y espera 20 s a un invitado que se está pausando.
		// TryLock y no Lock: quien llama a evictLRU ya tiene tomado el candado
		// de SU servicio, y un Lock aquí podría cruzarse con él.
		vlock := g.ensureLock(elegido)
		if !vlock.TryLock() {
			devolver()
			continue // esa esta ocupada; se prueba otra
		}

		err := freeze(id)
		vlock.Unlock()
		if err != nil {
			log.Printf("could not freeze %s to make room: %v", elegido, err)
			devolver()
			continue
		}
		return elegido
	}

	// Segundo escalón: el fondo de precalentadas. Son máquinas restauradas que
	// no atienden a nadie, así que su RAM es la más barata de recuperar, pero
	// hasta ahora nadie la pedía: evictOne existía, con sus tests, y no se
	// llamaba desde ningún sitio. El resultado era un 507 al cliente teniendo
	// memoria perfectamente liberable dormida en el fondo.
	//
	// Va después de las instancias de servicio y no antes a propósito: retirar
	// una precalentada le cuesta un arranque en frío a la SIGUIENTE petición de
	// ese servicio, mientras que congelar una instancia ociosa solo le cuesta un
	// thaw de milisegundos a la suya.
	if g.pool != nil && g.pool.evictOne(ctx) {
		return evictedPool
	}
	return ""
}

// reponerEntryLocked devuelve al mapa una instancia que se saco para congelar y
// al final no se congelo. Se llama con g.mu tomado.
func (g *Scheduler) reponerEntryLocked(service string, e *entry) {
	if g.services[service] == nil {
		g.services[service] = e
		return
	}
	for _, x := range g.extra[service] {
		if x.machineID == e.machineID {
			return // ya volvio por otro camino
		}
	}
	g.extra[service] = append(g.extra[service], e)
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
