package api

// GuestResyncPath es la ruta del agente de invitado que el daemon llama justo
// después de cada restauración (descongelar o instanciar de un snapshot).
//
// Existe porque todas las instancias de un mismo snapshot dorado despiertan con
// la MISMA memoria: el reloj de pared parado en el instante del volcado y el
// estado del CSPRNG del kernel copiado byte a byte. En Linux, Firecracker da al
// invitado VMGenID y el kernel resiembra solo, pero el reloj sigue parado; en
// macOS, Virtualization.framework no tiene VMGenID y dos réplicas devolvían los
// mismos "aleatorios" —ids de sesión MCP idénticos en microVMs independientes—.
const GuestResyncPath = "/resync"

// Límites de la entropía que acepta /resync. Por debajo no merece la pena
// resembrar; por encima no aporta nada y solo agranda lo que un llamante puede
// hacer leer al invitado.
const (
	GuestResyncMinEntropy = 32
	GuestResyncMaxEntropy = 512
	// GuestResyncEntropy es lo que manda el daemon: 512 bits, de sobra para
	// resembrar un ChaCha20 con clave de 256.
	GuestResyncEntropy = 64
	// GuestResyncMaxBody acota el cuerpo JSON: la entropía en base64 más el
	// sobre. Lo lee un PID 1 que no debe poder quedarse sin memoria por esto.
	GuestResyncMaxBody = 4096
)

// GuestResync es el cuerpo de POST /resync: la hora del host y entropía fresca.
type GuestResync struct {
	// UnixNano es el reloj de pared del host en el momento de enviar.
	UnixNano int64 `json:"unix_nano"`
	// Entropy son bytes aleatorios del host (base64 en el JSON).
	Entropy []byte `json:"entropy"`
	// Restore dice de qué restauración viene: ResyncThaw (la misma máquina
	// que se congeló) o ResyncInstance (una copia de un dorado: run -from,
	// fork). Los ganchos lo reciben en KLING_RESTORE. Vacío = daemon anterior.
	Restore string `json:"restore,omitempty"`
}

// Valores de GuestResync.Restore.
const (
	ResyncThaw     = "thaw"
	ResyncInstance = "instance"
)

// GuestResyncResult es la respuesta: cuánto estaba desfasado el reloj del
// invitado antes de corregirlo (positivo = iba atrasado). Solo diagnóstico.
type GuestResyncResult struct {
	SkewMS int64 `json:"skew_ms"`
	// Ready es el estado de "listo" justo tras resincronizar, sin ejecutar la
	// sonda (el último resultado que el agente recuerda) y con los ganchos ya
	// en marcha. nil = agente anterior a /ready. Ahorra al daemon una
	// petición en cada restauración.
	Ready *GuestReady `json:"ready,omitempty"`
	// Generation es el contador de restauraciones del invitado tras esta:
	// lo que el agente escribe en GuestGenerationFile. 0 = agente anterior.
	Generation uint64 `json:"generation,omitempty"`
	// Renewed son los ficheros de identidad que el agente renovó por ser una
	// copia (Restore = ResyncInstance): hoy, los machine-id con valor
	// (/etc/machine-id, /var/lib/dbus/machine-id). Issue #176.
	Renewed []string `json:"renewed,omitempty"`
}

// GuestGenerationFile es donde el agente deja, tras cada /resync, cuántas
// restauraciones lleva el invitado: un número en decimal y un salto de línea,
// escrito con un rename (quien lo vigile con inotify ve un IN_MOVED_TO). Es lo
// que en Firecracker hace VMGenID por el kernel, pero para los procesos: una app
// o un acompañante que lo vigila sabe que ahora es una copia y tiene que
// volver a sembrar lo que guarde en memoria (issue #175). Todas las copias de
// un dorado ven el mismo número: avisa de que hubo restauración, no es una
// identidad.
const GuestGenerationFile = "/run/kindling/generation"
