package api

// El servicio supervisado de una imagen.
//
// Una imagen puede declarar en su rootfs, en GuestServiceSpec, un proceso que
// el agente de invitado arranca después de montar los volúmenes y vigila: lo
// relanza si muere, lo para con su señal al apagar y guarda su salida. Es lo
// que hace con el ENTRYPOINT+CMD de una imagen de Docker el constructor "oci",
// con el usuario, el directorio y la señal de parada de la imagen. El entorno
// es el del agente (el de /entrypoint, que carga /etc/kling/env).

// GuestServiceSpec es dónde declara la imagen su servicio.
const GuestServiceSpec = "/etc/kindling/service.json"

// GuestServicePath (GET) es el estado del servicio supervisado (GuestService).
// GuestServiceStopPath (POST) lo para con su señal y espera a que salga (con
// SIGKILL al grupo pasado su plazo); ya no se relanza. Lo llama el daemon
// antes de vaciar los volúmenes y matar la máquina. Rutas de control: el
// gateway no las reenvía.
const (
	GuestServicePath     = "/service"
	GuestServiceStopPath = "/service/stop"
)

// GuestServiceLog es el fichero (dentro del invitado) con la salida del
// servicio; al pasar de GuestServiceLogMax se rota a .1.
const (
	GuestServiceLog    = "/var/log/kling-service.log"
	GuestServiceLogMax = 8 << 20
)

// Políticas de reinicio (ServiceSpec.Restart).
const (
	RestartAlways    = "always" // por defecto
	RestartOnFailure = "on-failure"
	RestartNo        = "no"
)

// ServiceSpec es el contenido de GuestServiceSpec.
type ServiceSpec struct {
	// Argv es el ejecutable y sus argumentos; sin "/" se busca en el PATH.
	Argv []string `json:"argv"`
	// User es "uid[:gid]" o "nombre[:grupo]" (de /etc/passwd y /etc/group de
	// la imagen); vacío = root. Como el USER de Docker.
	User string `json:"user,omitempty"`
	// WorkingDir es el directorio de trabajo (vacío = /).
	WorkingDir string `json:"working_dir,omitempty"`
	// StopSignal es la señal con la que se le pide parar ("SIGTERM" por
	// defecto, "SIGINT", "15", "SIGRTMIN+3"...: ParseSignal). Una que no se
	// entiende es SIGTERM, con un aviso: no deja la imagen sin servicio.
	// Pasado StopTimeoutSeconds (10 por defecto), SIGKILL a todo su grupo.
	StopSignal         string `json:"stop_signal,omitempty"`
	StopTimeoutSeconds int    `json:"stop_timeout_seconds,omitempty"`
	// Restart es RestartAlways, RestartOnFailure o RestartNo.
	Restart string `json:"restart,omitempty"`
	// ProbeTimeoutSeconds es el plazo de cada ejecución de la sonda de listo
	// (GuestReadyProbe; 0 = 10 s, como mucho MaxReadyTimeoutSeconds), y
	// ReadyStartPeriodSeconds lo que se le da de más al arranque antes de
	// darlo por lento (el impulso de CPU dura eso de más). Son el Timeout y
	// el StartPeriod del HEALTHCHECK de Docker. No es el ready_timeout_seconds
	// de commit, fork o run, que es la espera total a "listo".
	ProbeTimeoutSeconds     int `json:"probe_timeout_seconds,omitempty"`
	ReadyStartPeriodSeconds int `json:"ready_start_period_seconds,omitempty"`
}

// MaxReadyTimeoutSeconds acota ProbeTimeoutSeconds y ReadyStartPeriodSeconds:
// el HEALTHCHECK los admite de horas, pero "listo" es "terminó de arrancar" y
// una sonda colgada no puede tener esperando a quien la pregunta más que esto.
const MaxReadyTimeoutSeconds = 120

// GuestService es la respuesta de GET /service.
type GuestService struct {
	// Declared: la imagen declara un servicio. Sin él, el resto va vacío.
	Declared bool     `json:"declared"`
	Argv     []string `json:"argv,omitempty"`
	Running  bool     `json:"running"`
	PID      int      `json:"pid,omitempty"`
	// Starts cuenta los arranques (1 = no se ha relanzado nunca).
	Starts int `json:"starts"`
	// LastExit es cómo acabó la última vez ("exit status 1", "signal:
	// killed"); Error, por qué no se pudo lanzar.
	LastExit string `json:"last_exit,omitempty"`
	Error    string `json:"error,omitempty"`
	// Log son los últimos bytes de su salida (con ?tail=N, hasta 1 MiB).
	Log string `json:"log,omitempty"`
}
