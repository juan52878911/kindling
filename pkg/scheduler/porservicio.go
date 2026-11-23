package scheduler

import "time"

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
}
