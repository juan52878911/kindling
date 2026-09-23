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
}

// GuestResyncResult es la respuesta: cuánto estaba desfasado el reloj del
// invitado antes de corregirlo (positivo = iba atrasado). Solo diagnóstico.
type GuestResyncResult struct {
	SkewMS int64 `json:"skew_ms"`
}
