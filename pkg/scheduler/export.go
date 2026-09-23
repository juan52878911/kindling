package scheduler

import (
	"context"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Este fichero es la cara pública del planificador: lo que usa un producto que
// enruta peticiones a microVMs (el gateway MCP hoy, un sandbox mañana). Cada
// función envuelve una operación interna sin cambiar cómo toma los candados: la
// lógica y sus tests son los que tenía el gateway.

// New crea un planificador. prewarm es cuántas instancias precalentadas se
// mantienen por servicio en modo efímero (0 = ninguna).
func New(client *api.Client, idle time.Duration, ephemeral bool, prewarm int) *Scheduler {
	g := &Scheduler{
		client: client, idle: idle, Ephemeral: ephemeral,
		services: map[string]*entry{},
		extra:    map[string][]*entry{},
		routes:   map[string]*sessionRoute{},
	}
	g.pool = newPool(g, prewarm)
	g.pop = newPopularity(popularityPath())
	return g
}

// Client es el cliente del daemon con el que trabaja el planificador. Nil si no
// hay planificador: quien lo usa ya comprueba que haya cliente antes de hablar
// con el daemon.
func (g *Scheduler) Client() *api.Client {
	if g == nil {
		return nil
	}
	return g.client
}

// Idle es el tiempo sin uso tras el que una instancia se congela.
func (g *Scheduler) Idle() time.Duration { return g.idle }

// ErrTenantInstances es el error de Ensure/ScaleOut cuando el tenant ya tiene
// despiertas todas las instancias que su cuota permite.
var ErrTenantInstances = errTenantInstances

// ---- instancias

// Instance es una microVM despierta que sirve a un servicio.
type Instance = entry

// MachineID es el id de la máquina en el daemon.
func (e *entry) MachineID() string { return e.machineID }

// IP es la dirección del invitado en la red del host.
func (e *entry) IP() string { return e.ip }

// Proxy reenvía una petición HTTP al agente del invitado. No lleva la
// credencial del gateway: la quita antes de salir.
func (e *entry) Proxy() *httputil.ReverseProxy { return e.proxy }

// Ensure devuelve la instancia primaria del servicio, despertándola o creándola
// si hace falta. El tenant sale del contexto (ver WithTenant).
func (g *Scheduler) Ensure(ctx context.Context, service string) (*Instance, error) {
	return g.ensure(ctx, service)
}

// ScaleOut crea una réplica más del servicio.
func (g *Scheduler) ScaleOut(ctx context.Context, service string, t *Tenant) (*Instance, error) {
	return g.scaleOut(ctx, service, t)
}

// PickInstance devuelve una instancia con hueco para una sesión más, creando
// una réplica si todas están llenas.
func (g *Scheduler) PickInstance(ctx context.Context, service string, t *Tenant) (*Instance, error) {
	return g.pickInstance(ctx, service, t)
}

// Begin y End marcan una petición en vuelo sobre la instancia: mientras haya
// alguna, el segador no la congela aunque tarde más que Idle.
func (g *Scheduler) Begin(e *Instance) { g.begin(e) }
func (g *Scheduler) End(e *Instance)   { g.end(e) }

// Instance busca la instancia de un servicio que corre en machineID.
func (g *Scheduler) Instance(service, machineID string) *Instance {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.entryByMachineLocked(service, machineID)
}

// DropInstance olvida una instancia que ya no responde, para que la siguiente
// petición cree otra.
func (g *Scheduler) DropInstance(service, machineID string) {
	g.olvidarInstancia(service, machineID)
}

// ServiceStatus resume el estado de un servicio para mostrarlo.
type ServiceStatus struct {
	Prewarmed int           // precalentadas listas en el pool
	Warm      bool          // tiene instancia primaria despierta
	IP        string        // de la primaria
	Sessions  int           // sesiones fijadas al servicio
	Idle      time.Duration // desde el último uso de la primaria
}

// Status devuelve el estado del servicio en este momento.
func (g *Scheduler) Status(service string) ServiceStatus {
	st := ServiceStatus{Prewarmed: g.pool.stats()[service]}
	g.mu.Lock()
	defer g.mu.Unlock()
	if e, ok := g.services[service]; ok {
		st.Warm, st.IP, st.Idle = true, e.ip, time.Since(e.lastUse)
		for _, rt := range g.routes {
			if rt.service == service {
				st.Sessions++
			}
		}
	}
	return st
}

// ---- afinidad: una clave de sesión fija a una instancia

// Route es la instancia a la que está fijada una sesión.
type Route = sessionRoute

func (rt *sessionRoute) Service() string                                  { return rt.service }
func (rt *sessionRoute) MachineID() string                                { return rt.machineID }
func (rt *sessionRoute) IP() string                                       { return rt.ip }
func (rt *sessionRoute) Proxy() *httputil.ReverseProxy                    { return rt.proxy }
func (rt *sessionRoute) ServeHTTP(w http.ResponseWriter, r *http.Request) { rt.proxy.ServeHTTP(w, r) }

// Route devuelve a qué instancia está fijada la sesión key, o nil. Cuenta como
// uso de la sesión y de su instancia.
func (g *Scheduler) Route(key string) *Route { return g.route(key) }

// Bind fija la sesión key a la instancia e del servicio.
func (g *Scheduler) Bind(key, service string, e *Instance) { g.bind(key, service, e) }

// Rebind mueve una sesión existente a otra instancia (la anterior murió).
func (g *Scheduler) Rebind(key string, e *Instance) { g.rebind(key, e) }

// Forget suelta la sesión key.
func (g *Scheduler) Forget(key string) { g.forget(key) }

// ---- pool de precalentadas (modo efímero)

// Warm es una instancia restaurada y preparada (Prepare) que espera trabajo.
type Warm = warmVM

func (w *warmVM) ID() string    { return w.id }
func (w *warmVM) IP() string    { return w.ip }
func (w *warmVM) Token() string { return w.session }

// TakeWarm saca una instancia precalentada del servicio, o nil si no hay. No
// bloquea nunca.
func (g *Scheduler) TakeWarm(service string) *Warm { return g.pool.take(service) }

// FillPool repone en segundo plano las precalentadas del servicio.
func (g *Scheduler) FillPool(ctx context.Context, service, snapshot string) {
	g.pool.fill(ctx, service, snapshot)
}

// PoolStats cuenta las precalentadas listas por servicio.
func (g *Scheduler) PoolStats() map[string]int { return g.pool.stats() }

// ---- popularidad y tenants

// Observe anota una petición al servicio: guía qué se precalienta.
func (g *Scheduler) Observe(service string) { g.pop.observe(service) }

// Tenant es a quién se atribuye una petición (un token con nombre).
type Tenant = tenant

func (t *tenant) Name() string     { return t.name }
func (t *tenant) MaxInflight() int { return t.maxInflight }

// TenantFrom devuelve el tenant de la petición; el de por defecto si no hay.
func TenantFrom(ctx context.Context) *Tenant { return tenantFrom(ctx) }

// WithTenant asocia un tenant al contexto.
func WithTenant(ctx context.Context, t *Tenant) context.Context { return withTenant(ctx, t) }

// TenantBegin reserva un hueco de petición en vuelo para el tenant. false =
// ya está en su cuota y hay que contestar 429.
func (g *Scheduler) TenantBegin(t *Tenant) bool { return g.tenantBegin(t) }

// TenantEnd libera el hueco.
func (g *Scheduler) TenantEnd(t *Tenant) { g.tenantEnd(t) }

// AuthHandler envuelve h exigiendo el token (o uno de los tokens con nombre de
// SetTenants) y asocia el tenant a cada petición.
func (g *Scheduler) AuthHandler(h http.Handler, token string) http.Handler {
	return g.authHandler(h, token)
}

// WithoutGatewayCredential hace que p quite la cabecera Authorization antes de
// reenviar: el token del gateway es para el gateway, y un invitado —o un
// servidor externo enlazado— no tiene por qué verlo.
func WithoutGatewayCredential(p *httputil.ReverseProxy) { sinCredencialDelGateway(p) }

// ---- utilidades de red

// Alive dice si algo acepta conexiones en ip:port ahora mismo.
func Alive(ip string, port int) bool { return alive(ip, port) }

// WaitReady espera a que algo escuche en ip:port, o se rinde al agotar timeout.
func WaitReady(ctx context.Context, ip string, port int, timeout time.Duration) error {
	return waitReady(ctx, ip, port, timeout)
}

// ReadyTimeout es lo que el planificador espera a que una instancia nueva escuche.
const ReadyTimeout = readyTimeout
