package scheduler

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Este fichero es la SEMILLA de la ronda 8 (analisis/ronda8-ia/plan.md §2):
// solo declaraciones, cero comportamiento. Su dueño es Z3 (planificador por
// servicio); Z4 (gateway de IA) es quien rellena los ganchos desde
// aigw.Config.Backends. Si otra zona necesita cambiar una firma de aquí, lo pide
// a quien orquesta.
//
// Contrato común de todos los ganchos: nil = el comportamiento de hoy, y una
// respuesta cero del gancho también = el de hoy. Así el gateway MCP, que no pone
// ninguno, no cambia. "service" es la clave del servicio en el planificador (el
// dorado, para el gateway de IA). Se llaman con g.mu suelto y pueden llamarse
// desde varias goroutines a la vez: deben ser baratos y no bloquear.

// IdlePolicy es qué hace el segador con una instancia ociosa de un servicio
// (§1.4). Los textos coinciden con aigw.Idle*.
type IdlePolicy string

const (
	// IdleDefault ("") es lo de hoy: pausar dentro de PausedMiB y si no,
	// congelar.
	IdleDefault IdlePolicy = ""
	// IdleFreeze es lo de hoy, dicho explícitamente.
	IdleFreeze IdlePolicy = "freeze"
	// IdlePause pausa dentro del presupuesto de pausadas y, si no cabe o vence
	// PausedFor, BORRA. Nunca congela.
	IdlePause IdlePolicy = "pause"
	// IdleRemove borra la instancia ociosa en vez de congelarla. Además,
	// acquire salta el paso de descongelar para ese servicio y borra las
	// congeladas que encuentre: así toda instancia viva mapea el mem.file del
	// dorado (R8-01).
	IdleRemove IdlePolicy = "remove"
)

// Probe es una sonda HTTP de disponibilidad tras RESTAURAR una instancia (no
// tras reanudar una pausada ni descongelar). Se hace después de que el puerto
// acepte conexiones.
type Probe struct {
	// Path es la ruta GET, p. ej. "/healthz". Vacío = solo el dial TCP de hoy.
	Path string
	// Expect es el código que da por buena la sonda; 0 = 200.
	Expect int
	// Timeout es el plazo total de espera (dial + sonda); 0 = readyTimeout.
	Timeout time.Duration
}

// PorServicio son los ganchos que dejan a un llamante ajustar el planificador
// por servicio. Va incrustado en Scheduler, así que se ponen como
// s.PortFor = …, igual que MaxReplicasFor.
type PorServicio struct {
	// PortFor da el puerto del invitado de un servicio: el que se sondea en
	// esperarListo/alive y al que apunta el proxy. 0 = Port (o GuestPort).
	PortFor func(service string) int
	// MaxInflightFor da las peticiones en vuelo que aguanta una instancia de un
	// servicio antes de pedir otra réplica. 0 = MaxInflight.
	MaxInflightFor func(service string) int
	// IdleFor da la política de ociosidad de un servicio. IdleDefault = lo de
	// hoy.
	IdleFor func(service string) IdlePolicy
	// ReadyFor da la sonda HTTP tras restaurar una instancia de un servicio.
	// nil (o Path vacío) = el dial TCP de hoy.
	ReadyFor func(service string) *Probe
	// OnForget, si está, se llama con el servicio y la dirección (Addr del
	// puerto del servicio) de TODA instancia que el planificador deja de
	// tener: borrada, congelada, caída, TTL vencido o desalojada (R8-09). El
	// gateway de IA suelta ahí sus conexiones y su estado por réplica. Se
	// llama una vez por instancia y con g.mu suelto.
	OnForget func(service, addr string)

	// Ganchos de la ronda 9 (analisis/ronda9-carga/plan-r9.md). Semilla: están
	// declarados y nadie los llama todavía; los llama y los rellena Z4. El
	// contrato común sigue valiendo: nil = lo de hoy.

	// AfterRestore, si está, se llama con cada instancia recién RESTAURADA de
	// un dorado (no al reanudar, descongelar ni adoptar), cuando su puerto ya
	// acepta conexiones y ANTES de la sonda ReadyFor y de entregarla. También
	// para las del fondo y las de repuesto. El gateway de IA entrega ahí los
	// secretos por réplica (MMDS + ganchos). Un error suelta la instancia como
	// una que no llegó a escuchar, y su texto no puede llevar el secreto. A
	// diferencia de los demás ganchos puede tardar: se llama sin g.mu y con el
	// plazo de ctx.
	AfterRestore func(ctx context.Context, service string, mc *api.Machine) error
	// SpareFor da cuántas instancias de REPUESTO mantiene un servicio (C9-08):
	// restauradas, pasadas por AfterRestore y por WarmSpare, sin petición
	// asignada. 0 = ninguna. Un repuesto no cuenta como instancia ocupada ni
	// para MaxInflight; sí para la admisión de memoria. Al reclamarlo se
	// repone en segundo plano; al drenar o cerrar se borra (nunca se congela).
	SpareFor func(service string) int
	// WarmSpare, si está, se llama con cada repuesto antes de darlo por listo
	// (después de AfterRestore y de la sonda): el gateway de IA manda ahí la
	// petición de calentamiento del contrato para que la réplica toque sus
	// páginas fuera del camino de la petición. Un error descarta el repuesto.
	// Puede tardar, como AfterRestore.
	WarmSpare func(ctx context.Context, service, addr string) error
}

// adoptReadyTimeout es el plazo de la sonda de una máquina ADOPTADA (ya
// corría) cuando el servicio tiene ReadyFor. La adopción de la primaria se
// hace con el candado del servicio tomado: esperar el plazo entero dejaba en
// fila hasta 20 s a todas las peticiones de ese servicio (R8-08). Una máquina
// que ya corría o contesta enseguida o no está para atender.
const adoptReadyTimeout = 2 * time.Second

// portFor es el puerto del invitado del servicio: el de PortFor si lo da, si
// no Port (o GuestPort). Con g.mu suelto.
func (g *Scheduler) portFor(service string) int {
	if g.PortFor != nil {
		if p := g.PortFor(service); p > 0 {
			return p
		}
	}
	return g.port()
}

// puertoDe es el puerto del invitado de la instancia e: el que se fijó al
// crearla, o g.port() si no se fijó (entradas hechas a mano en los tests).
func (g *Scheduler) puertoDe(e *entry) int {
	if e.port > 0 {
		return e.port
	}
	return g.port()
}

// readyFor es la sonda HTTP del servicio, o nil si no la hay (sin gancho, el
// gancho da nil o Path vacío): entonces basta el dial TCP de siempre. Con g.mu
// suelto.
func (g *Scheduler) readyFor(service string) *Probe {
	if g.ReadyFor == nil {
		return nil
	}
	p := g.ReadyFor(service)
	if p == nil || p.Path == "" {
		return nil
	}
	return p
}

// plazoListo es cuánto se espera a que una instancia conseguida de la forma
// how quede lista: readyTimeout sin sonda; con ella, su Timeout (si lo da),
// recortado a adoptReadyTimeout al adoptar.
func plazoListo(sonda *Probe, how string) time.Duration {
	if sonda == nil {
		return readyTimeout
	}
	espera := readyTimeout
	if sonda.Timeout > 0 {
		espera = sonda.Timeout
	}
	if how == "adopt" && espera > adoptReadyTimeout {
		espera = adoptReadyTimeout
	}
	return espera
}

// clienteSonda hace las sondas de disponibilidad: sin redirecciones (el
// código que cuenta es el del invitado) ni conexiones que se queden abiertas
// hacia una instancia que quizá se descarta.
var clienteSonda = &http.Client{
	Transport: &http.Transport{DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// esperarSonda repite GET p.Path contra addr hasta que contesta p.Expect (200
// si es 0) o vence hasta. Cada intento va acotado a lo que quede del plazo y
// a un segundo como mucho, para que un invitado que acepta y no contesta no
// se coma el plazo de una vez.
func esperarSonda(ctx context.Context, addr string, p *Probe, hasta time.Time) error {
	want := p.Expect
	if want == 0 {
		want = http.StatusOK
	}
	path := p.Path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	url := "http://" + addr + path
	var last error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		resto := time.Until(hasta)
		if resto <= 0 {
			if last == nil {
				last = context.DeadlineExceeded
			}
			return fmt.Errorf("readiness probe %s: %w", path, last)
		}
		intento := min(resto, time.Second)
		rctx, cancel := context.WithTimeout(ctx, intento)
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
		if err != nil {
			cancel()
			return err
		}
		resp, err := clienteSonda.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
			if resp.StatusCode == want {
				cancel()
				return nil
			}
			err = fmt.Errorf("got %d, want %d", resp.StatusCode, want)
		}
		cancel()
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(50*time.Millisecond, max(time.Until(hasta), 0))):
		}
	}
}

// maxInflightPropio es el tope de peticiones en vuelo que el servicio da por
// MaxInflightFor, o 0 si no lo da (entonces se reparte como siempre, por
// sesiones y MaxInflight). Con g.mu suelto.
func (g *Scheduler) maxInflightPropio(service string) int {
	if g.MaxInflightFor == nil {
		return 0
	}
	return max(g.MaxInflightFor(service), 0)
}

// elegirPorTrabajo es elegirInstancia para un servicio repartido por trabajo
// (MaxInflightFor, R8-07): la carga de una instancia es lo que tiene en vuelo
// más sus reservas (peticiones ya elegidas que aún no han hecho Begin), y el
// tope de sesiones derivado de la memoria no cuenta: el producto no fija
// sesiones. Contar las reservas cierra el hueco entre elegir y Begin, en el
// que dos peticiones simultáneas leían la misma réplica libre. La elegida
// queda marcada porTrabajo (ver begin). Con g.mu tomado.
func elegirPorTrabajo(entries []*entry, tope int) (elegida, libre *entry) {
	var cargaLibre int
	for _, e := range entries {
		carga := e.inflight + e.reservasLocked()
		if carga < tope {
			e.porTrabajo = true
			return e, nil
		}
		if libre == nil || carga < cargaLibre {
			libre, cargaLibre = e, carga
		}
	}
	if libre != nil {
		libre.porTrabajo = true
	}
	return nil, libre
}

// idleFor es la política de ociosidad del servicio (IdleDefault sin gancho).
// Con g.mu suelto.
func (g *Scheduler) idleFor(service string) IdlePolicy {
	if g.IdleFor == nil {
		return IdleDefault
	}
	return g.IdleFor(service)
}

// sinCongelar dice si la política no congela nunca: lo ocioso se pausa o se
// borra, y lo congelado no se descongela (R8-01).
func sinCongelar(p IdlePolicy) bool { return p == IdlePause || p == IdleRemove }

// borrarCongeladas borra las congeladas ids del servicio que acquire apartó
// (marcadas en g.adquiriendo) en vez de descongelarlas, y las desmarca.
func (g *Scheduler) borrarCongeladas(ctx context.Context, service string, ids []string) {
	for _, id := range ids {
		if err := g.borrarConPlazo(ctx, id); err != nil {
			log.Printf("%s: could not remove frozen %s: %v", service, short(id), err)
		} else {
			log.Printf("%s: removed frozen %s (idle policy does not thaw)", service, short(id))
		}
		g.mu.Lock()
		delete(g.adquiriendo, id)
		g.mu.Unlock()
	}
}

// olvido es una instancia que salió del mapa, pendiente de avisar a OnForget.
type olvido struct{ service, addr string }

// encolarOlvidoLocked apunta que e (del servicio) salió del mapa, para avisar
// a OnForget cuando se suelte g.mu (avisarOlvidos). Sin gancho no apunta
// nada. Con g.mu tomado.
func (g *Scheduler) encolarOlvidoLocked(service string, e *entry) {
	if g.OnForget == nil {
		return
	}
	g.olvidos = append(g.olvidos, olvido{service, e.Addr(g.puertoDe(e))})
}

// avisarOlvidos llama a OnForget, con g.mu suelto, por cada instancia que
// salió del mapa desde el último aviso. Cada una sale de la cola una sola vez,
// así que no se avisa dos veces aunque dos goroutines avisen a la vez. Lo que
// encole un camino que no avisa sale en el siguiente aviso, como tarde en la
// siguiente vuelta del segador.
func (g *Scheduler) avisarOlvidos() {
	if g.OnForget == nil {
		return
	}
	g.mu.Lock()
	l := g.olvidos
	g.olvidos = nil
	g.mu.Unlock()
	for _, o := range l {
		g.OnForget(o.service, o.addr)
	}
}
