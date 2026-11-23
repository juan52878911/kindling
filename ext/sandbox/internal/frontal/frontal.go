// Package frontal es el servidor HTTP que se pone DELANTE de uno o varios
// daemons de kindling para ofrecer sandboxes a agentes de código.
//
// Existe porque el daemon de kindling escucha solo en un socket Unix y equivale
// a root en su host: no admite autenticación ni puede exponerse a la red. Igual
// que el gateway de MCP, este frontal es la única pieza que habla con la red, y
// lo que hace es autenticar, contabilizar y traducir: cada petición se
// convierte en una o varias llamadas al daemon por su cliente oficial.
//
// LO QUE ESTO NO ES. Los "tenants" de este paquete NO son aislamiento entre
// clientes. Todos los sandboxes de todos los tenants corren en los mismos
// daemons, en los mismos hosts, con el mismo kernel de host y la misma red de
// invitados. Lo que se garantiza es REPARTO Y CONTABILIDAD: un tenant no ve, no
// toca y no puede pasarse de su cuota de sandboxes, y cada máquina lleva
// etiquetado quién la creó. Si dos clientes no deben compartir un host —porque
// uno pudiera atacar al otro a través del hipervisor, del daemon o del
// scheduler— la respuesta no está aquí: son hosts separados, cada uno con su
// frontal o con su propio conjunto de tokens.
//
// El frontal no guarda NADA en disco. Todo lo que sabe de un sandbox lo saca de
// las etiquetas de la máquina en el daemon (kind=sandbox, tenant=..., template=...)
// y de sus relojes (TTLAt, FrozenAt). Así, reiniciar el frontal no pierde
// nada, dos frontales sobre los mismos hosts ven lo mismo, y `kling sandbox ls`
// en el host enseña exactamente la misma verdad. El store del daemon
// (PutStore/GetStore) no hace falta hoy: no hay ningún dato que no quepa en una
// etiqueta.
package frontal

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/consola"
	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/ext/sandbox/internal/inquilinos"
	"github.com/juan52878911/kindling/pkg/api"
)

// Etiquetas con las que el frontal marca sus máquinas en el daemon. Son el
// único "estado" del frontal, y por tanto el contrato con el paquete plantilla
// (que crea las instancias precalentadas) y con quien mire las máquinas a mano.
const (
	// LabelTenant es quién creó el sandbox. Una máquina kind=sandbox con
	// plantilla y SIN esta etiqueta es una instancia precalentada, libre para el
	// primero que la reclame (ver reclamarPrecalentada).
	LabelTenant = "tenant"
	// LabelTemplate es la plantilla de la que nació el sandbox.
	LabelTemplate = "template"
	// LabelReclamo es el nonce de la reclamación en curso de una precalentada
	// (ver reclamarUna). Va en el espacio reservado: un cliente no la pone ni
	// la ve.
	LabelReclamo = "kling.sandbox.claim"
)

// Tenant es un token con nombre y sus límites.
type Tenant struct {
	Nombre string
	Token  string
	// MaxSandboxes es cuántos sandboxes vivos puede tener a la vez (0 = sin
	// tope). MaxPorPlantilla acota lo mismo por plantilla.
	MaxSandboxes    int
	MaxPorPlantilla int
	// MaxShells es cuántas sesiones de shell interactivas puede tener abiertas
	// a la vez (0 = sin tope). Cuenta aparte de MaxSandboxes: una shell no es un
	// sandbox nuevo, pero sí un proceso del daemon y una conexión que este
	// frontal mantiene en pie, y ambos son un recurso que un tenant puede acaparar.
	MaxShells int
	// MaxProcesos y MaxSesiones acotan los procesos de fondo VIVOS y las
	// sesiones de exec de cada sandbox del tenant; MaxSeguir, las lecturas de
	// salida con follow abiertas a la vez en todo el tenant. 0 = el valor por
	// defecto de procesos.go.
	MaxProcesos int
	MaxSesiones int
	MaxSeguir   int
	// MaxMemMiB y MaxVCPUs acotan lo que un sandbox de este tenant puede pedir
	// (mem_mib y vcpus de la petición). 0 = el máximo por defecto del frontal
	// (Opciones.MaxMemMiB / Opciones.MaxVCPUs). Sin ellos, un solo token pide una
	// máquina del tamaño del host y deja sin memoria a los demás tenants aunque
	// MaxSandboxes sea 1.
	MaxMemMiB int
	MaxVCPUs  int
	// MaxMemTotalMiB acota la suma de mem_mib de todos los sandboxes vivos del
	// tenant, contando el tamaño EFECTIVO (el del snapshot de la plantilla
	// cuando la petición no dice nada). 0 = el del frontal
	// (Opciones.MaxMemTotalMiB), y si tampoco hay, sin tope total. Sin él, el
	// tope por sandbox no impide que un token llene la RAM del host con muchos
	// sandboxes medianos; es opt-in porque contarlo lista los hosts en cada
	// creación y solo suma los que contestan.
	MaxMemTotalMiB int
	// MaxLote es el mayor "count" de una creación en lote; 0 = el del frontal
	// (Opciones.MaxLote). Nunca pasa del global.
	MaxLote int
	// MaxVolumenes es cuántos volúmenes puede tener el tenant (0 = sin tope).
	MaxVolumenes int
	// Admin ve lo que es del operador y no de un tenant: /v1/hosts (los
	// endpoints de los daemons) y las métricas de TODOS los tenants. Sin él,
	// /v1/hosts es un 403 y /v1/metrics solo enseña las series del que llama.
	Admin bool
}

// Opciones configura el frontal.
type Opciones struct {
	Hosts   *hosts.Registro
	Tenants []Tenant
	// Almacen, si no es nil, aporta inquilinos persistentes con claves
	// creadas por la API (/v1/tenants). Conviven con Tenants, que mandan: un
	// nombre no puede estar en los dos.
	Almacen *inquilinos.Almacen

	// ImagenPorDefecto se usa cuando la petición no trae ni template ni image.
	// Vacía: la petición tiene que decir una de las dos.
	ImagenPorDefecto string
	// TTLPorDefecto en segundos; 0 = api.SandboxDefaultTTL.
	TTLPorDefecto int

	// MaxMemMiB y MaxVCPUs son el máximo de mem_mib y vcpus por sandbox para
	// los tenants que no traen el suyo. 0 = MaxMemMiBPorDefecto /
	// MaxVCPUsPorDefecto.
	MaxMemMiB int
	MaxVCPUs  int
	// MaxMemTotalMiB es la suma máxima de mem_mib vivos por tenant para los que
	// no traen la suya. 0 = sin tope total salvo el que traiga cada tenant.
	MaxMemTotalMiB int
	// MaxLote es el tope GLOBAL de "count" en POST /v1/sandboxes; ningún tenant
	// lo supera. 0 = MaxLotePorDefecto.
	MaxLote int
	// MaxVolumenMiB es el mayor size_mib de un volumen nuevo; 0 =
	// MaxVolumenMiBPorDefecto.
	MaxVolumenMiB int

	// Abandono es cuánto puede llevar un sandbox sin usarse antes de que la
	// limpieza lo borre. Es el segundo plazo, distinto del TTL: el TTL lo
	// duerme (on_ttl=freeze), este lo destruye. 0 = 24 h.
	Abandono time.Duration
	// CadaLimpieza es el periodo de la vuelta de limpieza. 0 = 5 min.
	CadaLimpieza time.Duration

	Log *log.Logger

	// Rellenador, si no es nil, recibe un aviso cada vez que una creación
	// reclama (o no encuentra) una precalentada, para que el fondo se reponga
	// sin esperar a su tic. *pool.Rellenador lo cumple.
	Rellenador Rellenador

	// Ingress configura la entrada HTTP a puertos del sandbox (ingress.go).
	// Con IngresoDominio vacío está apagada.
	Ingreso IngresoOpciones

	// SinConsola apaga la consola web (/console/, docs/consola.md): la ruta
	// contesta 404. El valor cero la deja encendida.
	SinConsola bool
}

// Rellenador es lo que el frontal necesita del fondo de precalentadas: poder
// avisarle de que en ese host falta de esa plantilla. No bloquea.
type Rellenador interface {
	Pedir(host, plantilla string)
}

// Servidor es el frontal. Implementa http.Handler.
type Servidor struct {
	reg     *hosts.Registro
	tenants []Tenant
	almacen *inquilinos.Almacen
	// defectos son los máximos del frontal que completan los topes a 0 de un
	// inquilino.
	defMem, defCPU, defTotal, defLote int
	imagen                            string
	ttl                               int
	abandono                          time.Duration
	cada                              time.Duration
	log                               *log.Logger
	mux                               *http.ServeMux
	ingreso                           *ingreso
	sinConsola                        bool

	maxVolumenMiB int

	// mu protege enVuelo y reclamos, y aparta las precalentadas que se reclaman. Las
	// cuotas se cuentan contra lo que el daemon tiene vivo, y una creación tarda
	// segundos en aparecer ahí: sin contar las que están en camino, N peticiones
	// simultáneas pasarían todas la cuota.
	mu      sync.Mutex
	enVuelo map[string]int
	// cerrojos: el de cuota de cada tenant, que serializa la cuenta de la
	// cuota con el fin de cada creación (ver reservar), y los de cada sandbox y
	// operación. Solo están las claves en uso (ver bloquear). Bajo mu.
	cerrojos map[string]*cerrojo
	// cargas son los files/replace en curso por tenant (ver entrarCarga).
	// Bajo mu.
	cargas map[string]int
	// trasContar, si no es nil, se llama en reservar entre contar y
	// comprobar. Solo lo usan los tests.
	trasContar func()
	// reclamos son las precalentadas que se están reclamando o se reclamaron
	// hace poco (ver reclamarPrecalentada); también bajo mu.
	reclamos map[string]reclamo
	// esperaReclamo es la pausa entre las dos fases de una reclamación (0:
	// esperaReclamo del paquete). Solo la cambian los tests.
	esperaReclamo time.Duration
	// rellena recibe los avisos de reposición (puede ser nil).
	rellena Rellenador
	// capReclamo cachea por host si su daemon anuncia sandbox-claim (ver
	// hostReclama). Bajo mCap, no bajo mu: se consulta en cada creación.
	mCap       sync.Mutex
	capReclamo map[string]capHost

	// shells cuenta las sesiones de shell abiertas por tenant AHORA MISMO. Sirve
	// dos propósitos con el mismo número: la cuota de Tenant.MaxShells y la
	// métrica kling_sandbox_shell_sessions de /v1/metrics.
	shells contadorTenant
	// seguir cuenta las lecturas de salida de procesos con follow abiertas.
	seguir contadorTenant
	// metricas acumula lo que /v1/metrics no puede sacar preguntando a los
	// daemons: cuántas creaciones ha habido y de qué tipo, y cuánto tardaron.
	metricas metricas
	// reconstruir vigila qué (host, plantilla) se están reconstruyendo ahora
	// mismo tras un fallo de TSC, para no lanzar la misma reconstrucción dos
	// veces (ver reconstruir.go).
	reconstruir reconstrucciones

	// rotos son los grafos del pool que la última vuelta de limpieza vio rotos
	// (a medias o mezclados). Solo se borra un roto que ya lo era en la vuelta
	// anterior (ver limpiarGrafos).
	rotosMu sync.Mutex
	rotos   map[string]bool
}

// nombreValido acota nombres de tenant y de host: van en etiquetas y en ids
// visibles ("host/máquina"), así que ni barras ni espacios ni vacíos.
var nombreValido = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// Máximos por sandbox cuando ni el tenant ni el frontal dicen otro. Holgados
// para un agente que compila o corre tests, y pequeños frente a un host: un
// tenant necesita muchos sandboxes, no uno enorme, para acaparar la máquina.
const (
	MaxMemMiBPorDefecto = 4096
	MaxVCPUsPorDefecto  = 4
	MaxLotePorDefecto   = 16
)

// minToken es la longitud mínima de un token de tenant. Uno corto se adivina
// probando, y el frontal es lo que se expone a la red.
const minToken = 16

// Nuevo construye el frontal. Falla si no hay ningún tenant con token: este
// servidor se expone a la red y sin token sería el daemon a pelo.
func Nuevo(o Opciones) (*Servidor, error) {
	if o.Hosts == nil {
		return nil, errors.New("no hosts registry")
	}
	for _, h := range o.Hosts.Todos() {
		if !nombreValido.MatchString(h.Nombre) {
			return nil, fmt.Errorf("host name %q is not valid (letters, digits, '.', '_', '-')", h.Nombre)
		}
	}
	if o.MaxMemMiB < 0 || o.MaxVCPUs < 0 || o.MaxMemTotalMiB < 0 {
		return nil, errors.New("default mem_mib and vcpus limits can't be negative")
	}
	if o.MaxLote < 0 || o.MaxVolumenMiB < 0 {
		return nil, errors.New("MaxLote and MaxVolumenMiB can't be negative")
	}
	maxLote := o.MaxLote
	if maxLote == 0 {
		maxLote = MaxLotePorDefecto
	}
	maxVol := o.MaxVolumenMiB
	if maxVol == 0 {
		maxVol = MaxVolumenMiBPorDefecto
	}
	maxMem, maxCPU, maxTotal := o.MaxMemMiB, o.MaxVCPUs, o.MaxMemTotalMiB
	if maxMem == 0 {
		maxMem = MaxMemMiBPorDefecto
	}
	if maxCPU == 0 {
		maxCPU = MaxVCPUsPorDefecto
	}
	vistos, tokens := map[string]bool{}, map[string]string{}
	var tenants []Tenant
	for _, t := range o.Tenants {
		if !nombreValido.MatchString(t.Nombre) {
			return nil, fmt.Errorf("tenant name %q is not valid (letters, digits, '.', '_', '-')", t.Nombre)
		}
		if vistos[t.Nombre] {
			return nil, fmt.Errorf("tenant %q is configured twice", t.Nombre)
		}
		vistos[t.Nombre] = true
		// Un tenant sin token no protege nada: mejor negarse a arrancar que
		// registrar una entrada que aceptaría la cadena vacía.
		if t.Token == "" {
			return nil, fmt.Errorf("tenant %q has an empty token", t.Nombre)
		}
		if len(t.Token) < minToken {
			return nil, fmt.Errorf("tenant %q: its token is %d characters; the minimum is %d", t.Nombre, len(t.Token), minToken)
		}
		// Dos tenants con el mismo token: auth se queda con el último que
		// coincide, y el otro tenant actúa como este sin saberlo.
		if otro, ok := tokens[t.Token]; ok {
			return nil, fmt.Errorf("tenants %q and %q share a token", otro, t.Nombre)
		}
		tokens[t.Token] = t.Nombre
		if t.MaxSandboxes < 0 || t.MaxPorPlantilla < 0 || t.MaxShells < 0 || t.MaxProcesos < 0 || t.MaxSesiones < 0 || t.MaxSeguir < 0 || t.MaxMemMiB < 0 || t.MaxVCPUs < 0 || t.MaxMemTotalMiB < 0 {
			return nil, fmt.Errorf("tenant %q: limits can't be negative", t.Nombre)
		}
		if t.MaxLote < 0 || t.MaxVolumenes < 0 {
			return nil, fmt.Errorf("tenant %q: limits can't be negative", t.Nombre)
		}
		tenants = append(tenants, completar(t, maxMem, maxCPU, maxTotal, maxLote))
	}
	if o.Almacen != nil {
		for _, t := range o.Almacen.Listar() {
			if vistos[t.Nombre] {
				return nil, fmt.Errorf("tenant %q is both in the environment and in the tenant store", t.Nombre)
			}
		}
	}
	if len(tenants) == 0 && (o.Almacen == nil || o.Almacen.Cuantos() == 0) {
		return nil, errors.New("no tenants configured: the frontal refuses to serve without authentication")
	}
	s := &Servidor{
		reg: o.Hosts, tenants: tenants, imagen: o.ImagenPorDefecto,
		ttl: o.TTLPorDefecto, abandono: o.Abandono, cada: o.CadaLimpieza,
		log: o.Log, enVuelo: map[string]int{}, maxVolumenMiB: maxVol,
		rellena: o.Rellenador, capReclamo: map[string]capHost{}, sinConsola: o.SinConsola, almacen: o.Almacen, defMem: maxMem, defCPU: maxCPU, defTotal: maxTotal, defLote: maxLote,
	}
	if s.ttl == 0 {
		s.ttl = api.SandboxDefaultTTL
	}
	if s.ttl < 1 || s.ttl > api.SandboxMaxTTL {
		return nil, fmt.Errorf("default ttl must be between 1 and %d seconds", api.SandboxMaxTTL)
	}
	if s.abandono == 0 {
		s.abandono = 24 * time.Hour
	}
	if s.cada == 0 {
		s.cada = 5 * time.Minute
	}
	if s.log == nil {
		s.log = log.Default()
	}
	for _, t := range s.tenantsTodos() {
		s.avisarCuotasAbiertas(t)
	}
	s.rutas()
	ing, err := nuevoIngreso(s, o.Ingreso)
	if err != nil {
		return nil, err
	}
	s.ingreso = ing
	return s, nil
}

// avisarCuotasAbiertas deja en el log un inquilino sin tope de sandboxes ni de
// memoria total: solo lo acota cada petición, así que puede llenar los hosts.
// Sale al arrancar y cada vez que se crea o edita uno.
func (s *Servidor) avisarCuotasAbiertas(t Tenant) {
	if t.MaxSandboxes == 0 && t.MaxMemTotalMiB == 0 {
		s.log.Printf("frontal: warning: tenant %q has neither MaxSandboxes nor MaxMemTotalMiB: its sandboxes are limited only per request, so it can fill the hosts", t.Nombre)
	}
}

func (s *Servidor) rutas() {
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/health", s.handleHealth)
	m.Handle("GET /v1/hosts", s.auth(http.HandlerFunc(s.handleHosts)))
	// /v1/hosts es solo para admin (handleHosts). Cualquier token de tenant
	// vale para /v1/metrics y /v1/templates: no dan nada de OTROS tenants (las
	// series por tenant solo las ve entero un admin; el resto, el catálogo de
	// plantillas y los totales agregados), y exigir un token distinto solo
	// obligaría a repartir un segundo secreto sin ganar nada a cambio.
	m.Handle("GET /v1/metrics", s.auth(http.HandlerFunc(s.handleMetrics)))
	m.Handle("GET /v1/templates", s.auth(http.HandlerFunc(s.handleTemplates)))
	// Solo admin (lo comprueba handlePool): el operador de Kubernetes fija con
	// ella el fondo de una plantilla (SandboxPool).
	m.Handle("PUT /v1/templates/{name}/pool", s.auth(http.HandlerFunc(s.handlePool)))
	m.Handle("POST /v1/sandboxes", s.auth(http.HandlerFunc(s.handleCrear)))
	m.Handle("GET /v1/sandboxes", s.auth(http.HandlerFunc(s.handleListar)))
	// Las rutas por sandbox se despachan a mano: el id lleva una barra dentro
	// ("host/máquina") y el ServeMux no deja que un comodín la contenga.
	m.Handle("/v1/sandboxes/", s.auth(http.HandlerFunc(s.handleSandbox)))
	m.Handle("/v1/volumes", s.auth(http.HandlerFunc(s.handleVolumenes)))
	m.Handle("/v1/volumes/", s.auth(http.HandlerFunc(s.handleVolumenes)))
	// Grafos precalentados (grafos.go): reclamar, listar, ver y soltar.
	m.Handle("POST /v1/graphs", s.auth(http.HandlerFunc(s.handleCrearGrafo)))
	m.Handle("GET /v1/graphs", s.auth(http.HandlerFunc(s.handleListarGrafos)))
	m.Handle("/v1/graphs/", s.auth(http.HandlerFunc(s.handleGrafo)))
	s.rutasTenants(m)
	if !s.sinConsola {
		m.Handle(consola.Prefijo, consola.Handler()) // consola web (docs/consola.md)
	}
	s.mux = m
}

// ServeHTTP despacha.
//
// Con la entrada a puertos activa, una petición cuyo Host es
// <puerto>-<id>.<dominio> va al proxy y no al mux de la API.
func (s *Servidor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.ingreso != nil && s.ingreso.esDeIngreso(r) {
		s.ingreso.servir(w, r)
		return
	}
	// Lo que el mux no casa (404) o casa con otro método (405) contesta texto
	// plano; solo en ese caso se envuelve para darlo como JSON.
	if _, patron := s.mux.Handler(r); patron == "" {
		w = &escritorJSON{ResponseWriter: w, r: r}
	}
	s.mux.ServeHTTP(w, r)
}

// ---- autenticación

type claveTenant struct{}

// auth exige un Bearer que coincida con el token de algún tenant y deja el
// tenant en el contexto. Todos los tokens se comparan siempre, sin cortar en el
// primer acierto, para que el tiempo de respuesta no delate cuál coincidió.
// ConstantTimeCompare ya devuelve 0 cuando difieren las longitudes.
func (s *Servidor) auth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := bearer(r.Header.Get("Authorization"))
		if !ok {
			authFail(w)
			return
		}
		t := s.resolver(got)
		if t == nil {
			authFail(w)
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claveTenant{}, t)))
	})
}

// resolver da el tenant al que abre el token: primero los del entorno (todos
// comparados, sin cortar) y luego las claves del almacén. nil si no abre nada.
func (s *Servidor) resolver(tok string) *Tenant {
	var t *Tenant
	for i := range s.tenants {
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.tenants[i].Token)) == 1 {
			t = &s.tenants[i]
		}
	}
	if t == nil && s.almacen != nil {
		if q, ok := s.almacen.Resolver(tok); ok {
			v := s.deAlmacen(q)
			t = &v
		}
	}
	return t
}

// deAlmacen convierte un inquilino del almacén en Tenant, con los topes por
// defecto del frontal ya aplicados (como hace Nuevo con los del entorno).
func (s *Servidor) deAlmacen(q inquilinos.Inquilino) Tenant {
	p := q.Topes
	return completar(Tenant{
		Nombre: q.Nombre, Admin: q.Admin,
		MaxSandboxes: p.MaxSandboxes, MaxPorPlantilla: p.MaxPorPlantilla, MaxShells: p.MaxShells,
		MaxProcesos: p.MaxProcesos, MaxSesiones: p.MaxSesiones, MaxSeguir: p.MaxSeguir,
		MaxMemMiB: p.MaxMemMiB, MaxVCPUs: p.MaxVCPUs, MaxMemTotalMiB: p.MaxMemTotalMiB,
		MaxLote: p.MaxLote, MaxVolumenes: p.MaxVolumenes,
	}, s.defMem, s.defCPU, s.defTotal, s.defLote)
}

// completar rellena con los máximos del frontal los topes que el tenant deja a 0.
func completar(t Tenant, maxMem, maxCPU, maxTotal, maxLote int) Tenant {
	if t.MaxMemMiB == 0 {
		t.MaxMemMiB = maxMem
	}
	if t.MaxVCPUs == 0 {
		t.MaxVCPUs = maxCPU
	}
	if t.MaxMemTotalMiB == 0 {
		t.MaxMemTotalMiB = maxTotal
	}
	if t.MaxLote == 0 || t.MaxLote > maxLote {
		t.MaxLote = maxLote
	}
	return t
}

// tenantsTodos son los del entorno y los del almacén.
func (s *Servidor) tenantsTodos() []Tenant {
	out := append([]Tenant(nil), s.tenants...)
	if s.almacen != nil {
		for _, q := range s.almacen.Listar() {
			out = append(out, s.deAlmacen(q))
		}
	}
	return out
}

// tenantNombre busca un tenant por nombre, en el mismo orden que tenantsTodos
// (primero los fijos), pero sin copiar el almacén entero con sus claves: el
// ingress lo llama en cada petición (D-06).
func (s *Servidor) tenantNombre(n string) *Tenant {
	for i := range s.tenants {
		if s.tenants[i].Nombre == n {
			t := s.tenants[i]
			return &t
		}
	}
	if s.almacen != nil {
		if q, err := s.almacen.Obtener(n); err == nil {
			t := s.deAlmacen(q)
			return &t
		}
	}
	return nil
}

func bearer(h string) (string, bool) {
	const p = "bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(p):])
	return tok, tok != ""
}

func authFail(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="kindling-sandbox"`)
	fail(w, http.StatusUnauthorized, errors.New("missing Authorization header: Bearer <token>, or the token is invalid"))
}

// tenantDe devuelve el tenant que auth dejó en el contexto. Nunca es nil detrás
// de auth; si lo fuera es un error de programación, no una petición anónima.
func tenantDe(r *http.Request) *Tenant {
	t, _ := r.Context().Value(claveTenant{}).(*Tenant)
	return t
}

// ---- rutas sueltas

func (s *Servidor) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Sin token (es la sonda de vida), así que no dice nada más que "vivo":
	// cuántos hosts hay detrás es del operador.
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleHosts enseña los daemons de detrás con sus endpoints: rutas de
// sockets y direcciones internas que son del operador, no de un tenant.
func (s *Servidor) handleHosts(w http.ResponseWriter, r *http.Request) {
	if !tenantDe(r).Admin {
		fail(w, http.StatusForbidden, errors.New("listing the hosts needs an admin token"))
		return
	}
	writeJSON(w, http.StatusOK, s.reg.Estados(r.Context()))
}

// ---- respuestas

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// codigoDeHost traduce el error de hosts.Intentar a un estado HTTP.
func codigoDeHost(err error) int {
	switch {
	case errors.Is(err, hosts.ErrSinHosts), errors.Is(err, hosts.ErrSinSitio):
		return http.StatusServiceUnavailable
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	}
	var se *api.StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return http.StatusBadGateway
}
