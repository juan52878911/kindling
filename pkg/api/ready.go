package api

// "Listo" definido por la imagen y ganchos tras restaurar.
//
// Hasta ahora "la máquina está lista" era "algo contesta en el puerto del
// agente" (GuestPort). Para un servidor MCP es verdad; para un invitado que
// arranca algo grande por detrás del agente no: Android contesta en el 8080 a
// los 1,4 s y termina de arrancar (sys.boot_completed) a los 3–10 s. Un dorado
// guardado en medio despierta a medio arrancar en cada copia.
//
// La imagen lo declara con dos rutas de su propio rootfs, que el agente de
// invitado (pkg/guest) ejecuta él mismo, sin allow_exec y sin argumentos de
// nadie:
//
//   - GuestReadyProbe: un ejecutable que sale con 0 cuando el invitado está
//     listo. Una vez listo, se recuerda (es "terminó de arrancar", no un
//     chequeo de vida).
//   - GuestPostRestoreDir: ejecutables que corren, en orden, tras cada
//     restauración, cuando el daemon ya resincronizó reloj y entropía, montó
//     los volúmenes y entregó las credenciales a MMDS (POST /hooks). Mientras
//     corren, el invitado no está listo.
//
// Sin ninguna de las dos, el invitado está listo en cuanto contesta: lo de
// siempre.

// Rutas del agente de invitado. Las dos son de control: el gateway no debe
// reenviárselas a sus clientes (pkg/guest.IsControlPath).
const (
	// GuestReadyPath (GET) dice si el invitado está listo (GuestReady). 200
	// si lo está, 503 si no; el cuerpo es el mismo en los dos casos.
	GuestReadyPath = "/ready"
	// GuestHooksPath (POST) vuelve a ejecutar los ganchos tras restaurar: para
	// cuando lo que necesitan llega después de restaurar (un secreto por
	// MMDS). 202 si arrancan, 409 si ya estaban corriendo.
	GuestHooksPath = "/hooks"
)

// Rutas DENTRO del invitado donde la imagen declara la sonda y los ganchos.
const (
	GuestReadyProbe     = "/etc/kindling/ready"
	GuestPostRestoreDir = "/etc/kindling/post-restore.d"
)

// Estados de los ganchos tras restaurar (GuestReady.Hooks).
const (
	HooksNone    = ""        // la imagen no declara ganchos, o no han corrido
	HooksRunning = "running" // corriendo: el invitado no está listo
	HooksDone    = "done"    // terminaron todos con 0
	HooksFailed  = "failed"  // alguno falló o agotó su plazo: no se reintenta solo
)

// GuestReady es la respuesta de GET /ready del agente.
type GuestReady struct {
	// Ready: la sonda (si la hay) contestó 0 y ningún gancho está corriendo
	// ni falló.
	Ready bool `json:"ready"`
	// Probe: la imagen declara una sonda (GuestReadyProbe).
	Probe bool `json:"probe,omitempty"`
	// HasHooks: la imagen declara ganchos tras restaurar (GuestPostRestoreDir).
	// El daemon los lanza con POST /hooks al final de cada restauración.
	HasHooks bool `json:"has_hooks,omitempty"`
	// Hooks es el estado de la última tanda de ganchos (HooksRunning, ...).
	Hooks string `json:"hooks,omitempty"`
	// Detail explica por qué no está listo: la salida (recortada) de la sonda
	// o del gancho que falló.
	Detail string `json:"detail,omitempty"`
}

// Declares dice si la imagen declara algo (sonda o ganchos): sin nada, "listo"
// no aporta más que "el agente contesta".
func (g GuestReady) Declares() bool { return g.Probe || g.HasHooks || g.Hooks != HooksNone }

// Estados de Machine.Ready, lo que el daemon sabe de la última vez que miró.
const (
	ReadyUnknown = ""        // sin sonda ni ganchos, agente viejo, o nadie ha mirado
	ReadyWaiting = "waiting" // la imagen declara sonda o ganchos y aún no está
	ReadyYes     = "ready"
	ReadyFailed  = "failed" // un gancho falló
)

// ReadyResult es la respuesta de GET /machines/{ref}/ready.
type ReadyResult struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Ready string `json:"ready"` // ReadyYes, ReadyWaiting, ReadyFailed o "" (nada que esperar)
	// Guest es lo que contestó el agente; nil si no hay agente o es anterior
	// a /ready.
	Guest *GuestReady `json:"guest,omitempty"`
	// WaitedMS es cuánto se esperó en esta llamada.
	WaitedMS int64 `json:"waited_ms"`
}

// OK dice si no hay nada que esperar: listo, o una imagen que no declara nada.
func (r ReadyResult) OK() bool { return r.Ready == ReadyYes || r.Ready == ReadyUnknown }

// ReadyMaxWaitSeconds es el tope de ?wait= en GET /machines/{ref}/ready y de
// RunRequest.ReadyTimeoutSeconds.
const ReadyMaxWaitSeconds = 30 * 60

// GuestMemInfoPath (GET) es la memoria del invitado vista desde dentro
// (/proc/meminfo), en GuestMemInfo. Ruta de control. La usa squeeze en macOS,
// donde el globo de Virtualization.framework no da estadísticas: sin ella
// apretaba a ciegas hasta la mitad de la RAM y un Android se quedaba sin nada.
const GuestMemInfoPath = "/meminfo"

// GuestMemInfo es la respuesta de GET /meminfo.
type GuestMemInfo struct {
	TotalMiB     int `json:"total_mib"`
	AvailableMiB int `json:"available_mib"`
}
