package credproxy

// Registro de auditoría del proxy: una línea JSON por petición (o, más
// adelante, por conexión de otros proxies de credenciales) en un fichero por
// máquina. Responde a "¿qué hizo el invitado con la clave?" sin mirar dentro:
// método, host, ruta enmascarada, estado, qué credenciales se usaron, bytes y
// duración. Qué NO se escribe nunca: la clave, el marcador, cuerpos, cabeceras,
// el contenido de la query ni el texto de un error del proveedor.
//
// EL CAMINO CALIENTE no espera al disco: la petición deja su registro en un
// canal con un envío que no bloquea, y una sola goroutine lo codifica y lo
// escribe con buffer (se vacía en cuanto la cola queda vacía, o cada auditLote
// registros si no para de llegar, y cada segundo por si acaso; sin fsync: es
// observabilidad, no un diario transaccional, y lo que ya se escribió sobrevive
// a que maten el proceso). Si el canal está
// lleno —el disco no da abasto o un invitado dispara a saco— el registro se
// descarta y se cuenta; la cuenta viaja en el campo dropped del siguiente
// registro que sí se escriba (o en uno de tipo "dropped" si no llega ninguno),
// así que una pérdida nunca pasa callada.
//
// EL FICHERO vive en el directorio de la máquina, que en Linux es del usuario
// sin privilegios del VMM mientras el proxy corre como root: se abre con
// O_NOFOLLOW y se exige que sea un fichero regular, para que un enlace o una
// FIFO plantados ahí no lleven la escritura a otro sitio ni la bloqueen. Se
// rota al pasar de AuditConfig.MaxBytes: <fichero> pasa a .1, .1 a .2... hasta
// AuditConfig.Generations. Lo que se cae de la última generación tampoco pasa
// callado: sus líneas (y los descartados que ya llevaban) se suman a dropped
// del siguiente registro, y también a rotated para saber por qué.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// AuditFile es el nombre del registro en el directorio de la máquina.
	AuditFile = "credaudit.jsonl"
	// AuditMaxBytes es el tamaño por defecto a partir del cual se rota.
	AuditMaxBytes = 4 << 20
	// AuditGenerations es cuántos ficheros rotados (.1, .2...) se guardan por
	// defecto. Con AuditMaxBytes, ~16 MiB por máquina: unas 80 000 peticiones
	// antes de que la más antigua se caiga (y se cuente).
	AuditGenerations = 3
	// maxAuditMiB y maxAuditGenerations acotan lo configurable.
	maxAuditMiB         = 1024
	maxAuditGenerations = 99

	// KindHTTP es un registro del proxy HTTP.
	KindHTTP = "http"
	// KindDropped es un registro que solo lleva la cuenta de descartados:
	// se escribe cuando hubo pérdidas y no llegó otro registro que la lleve.
	KindDropped = "dropped"

	// auditCola es cuántos registros esperan a la goroutine escritora antes de
	// empezar a descartar.
	auditCola = 1024
	// auditLote y auditCada: cada cuánto se vacía el buffer al disco.
	auditLote = 64
	auditCada = time.Second
	// auditReintento: tras un fallo al abrir, cuánto se espera para volver a
	// intentarlo (mientras, los registros se cuentan como descartados).
	auditReintento = time.Second
)

// Motivos de un registro (Record.Reason). "" es que la petición llegó al
// proveedor y su respuesta al invitado entera.
const (
	ReasonDisabled      = "disabled"       // el proxy no está activo (macOS fuera de allowlist)
	ReasonBusy          = "busy"           // MaxInFlight peticiones en vuelo
	ReasonNoCredential  = "no_credential"  // el Host no tiene credencial
	ReasonConnect       = "connect"        // CONNECT: no se admite
	ReasonAmbiguousPath = "ambiguous_path" // ruta ambigua con Allow de por medio
	ReasonNotAllowed    = "not_allowed"    // ninguna credencial permite método y ruta
	ReasonBodyTooLarge  = "body_too_large" // cuerpo por encima de MaxBody
	ReasonBadBody       = "bad_body"       // no se pudo leer el cuerpo del invitado
	ReasonBadRequest    = "bad_request"    // no se pudo construir la petición saliente
	ReasonUpstreamError = "upstream_error" // el proveedor no contestó
	ReasonBadEncoding   = "bad_encoding"   // Content-Encoding que no se puede redactar
	ReasonAborted       = "aborted"        // la respuesta se cortó a medias
)

// Record es una línea del registro. Los campos de base de datos (User,
// Database, Auth) son para proxies de otros protocolos: el HTTP no los rellena.
// Ningún campo lleva la clave, el marcador ni contenido de la petición: Path va
// enmascarada (ver rutaAuditada) y de la query solo se dice si la había.
type Record struct {
	TS     time.Time `json:"ts"`
	Kind   string    `json:"kind"`
	Method string    `json:"method,omitempty"`
	Host   string    `json:"host,omitempty"`
	Path   string    `json:"path,omitempty"`
	Query  bool      `json:"query,omitempty"`
	Status int       `json:"status,omitempty"`
	Reason string    `json:"reason,omitempty"`
	// Denied: la petición se rechazó por política (sin credencial para el
	// Host, Allow que no la permite, ruta ambigua o proxy inactivo), no por un
	// límite o un fallo.
	Denied bool `json:"denied,omitempty"`
	// Creds son los Env de las credenciales cuyo marcador se sustituyó de
	// verdad en esta petición.
	Creds    []string `json:"creds,omitempty"`
	User     string   `json:"user,omitempty"`
	Database string   `json:"database,omitempty"`
	Auth     string   `json:"auth,omitempty"`
	// AnyDatabase: la credencial usada deja entrar en cualquier base (sin
	// -database, o de un almacén anterior a que fuese obligatoria).
	AnyDatabase bool `json:"any_database,omitempty"`
	// Upstream es la dirección fijada por el operador a la que marcó el
	// proxy de Postgres (configuración, no un secreto); vacío si marcó el
	// dominio de la credencial.
	Upstream  string `json:"upstream,omitempty"`
	ReqBytes  int64  `json:"req_bytes"`
	RespBytes int64  `json:"resp_bytes"`
	MS        int64  `json:"ms"`
	// Dropped: registros descartados antes de este (cola llena, fallo al
	// escribir o caídos de la última generación al rotar).
	Dropped uint64 `json:"dropped,omitempty"`
	// Rotated: de los Dropped, cuántos se perdieron al rotar (ver
	// AuditConfig): si no para de subir, conviene más tamaño o generaciones.
	Rotated uint64 `json:"rotated,omitempty"`
}

// AuditConfig es el tamaño de cada fichero del registro y cuántos rotados se
// guardan. El valor cero son los de por defecto (AuditMaxBytes,
// AuditGenerations).
type AuditConfig struct {
	MaxBytes    int64
	Generations int
}

func (c AuditConfig) conDefectos() AuditConfig {
	if c.MaxBytes <= 0 {
		c.MaxBytes = AuditMaxBytes
	}
	if c.Generations <= 0 {
		c.Generations = AuditGenerations
	}
	return c
}

// String es "MiB:generaciones", lo que lee ParseAuditConfig (el daemon se lo
// pasa así a kling-vz).
func (c AuditConfig) String() string {
	c = c.conDefectos()
	return fmt.Sprintf("%d:%d", c.MaxBytes>>20, c.Generations)
}

// ParseAuditConfig lee "MiB:generaciones" (1-1024 MiB, 1-99 generaciones).
func ParseAuditConfig(s string) (AuditConfig, error) {
	mib, gens, ok := strings.Cut(s, ":")
	m, err1 := strconv.Atoi(mib)
	g, err2 := strconv.Atoi(gens)
	if !ok || err1 != nil || err2 != nil || m < 1 || m > maxAuditMiB || g < 1 || g > maxAuditGenerations {
		return AuditConfig{}, fmt.Errorf("credential audit %q: want MIB:GENERATIONS (1-%d MiB, 1-%d generations)", s, maxAuditMiB, maxAuditGenerations)
	}
	return AuditConfig{MaxBytes: int64(m) << 20, Generations: g}, nil
}

// AuditFiles son los ficheros del registro de path que existen, del más
// antiguo al actual (path.N ... path.1, path). Lo usa quien lo lee sin saber
// con cuántas generaciones se escribió.
func AuditFiles(path string) []string {
	var out []string
	for i := maxAuditGenerations; i >= 1; i-- {
		f := path + "." + strconv.Itoa(i)
		if _, err := os.Lstat(f); err == nil {
			out = append(out, f)
		}
	}
	if _, err := os.Lstat(path); err == nil {
		out = append(out, path)
	}
	return out
}

// Auditor escribe registros en un fichero sin bloquear a quien los manda. Lo
// crea NewAuditor y lo usa el Proxy (y lo podrá usar cualquier otro proxy de
// credenciales de la máquina, que comparte así el fichero y la cuenta de
// descartados).
type Auditor struct {
	path    string
	cfg     AuditConfig // cero = por defecto (ver AuditConfig)
	logf    func(string, ...any)
	ch      chan Record
	dropped atomic.Uint64
	// rotados es, de dropped, lo que se cayó al rotar (solo la escritora).
	rotados uint64
	quit    chan struct{}
	done    chan struct{}
	cerrar  sync.Once
	// espera, si no es nil, corre antes de escribir cada registro: los tests
	// la usan para atascar la escritora.
	espera func()

	// De aquí abajo, solo de la goroutine escritora.
	f         *os.File
	bw        *bufio.Writer
	tam       int64
	enBuffer  uint64
	noAntesDe time.Time
	ultimoLog time.Time
}

// NewAuditor arranca la escritora de path. logf (puede ser nil) recibe los
// fallos de disco, como mucho uno por minuto.
func NewAuditor(path string, logf func(string, ...any)) *Auditor {
	return nuevoAuditor(path, logf, nil)
}

// NewAuditorCon es NewAuditor con un tamaño y unas generaciones propios.
func NewAuditorCon(path string, cfg AuditConfig, logf func(string, ...any)) *Auditor {
	a := nuevoAuditor(path, logf, nil)
	a.cfg = cfg
	return a
}

func nuevoAuditor(path string, logf func(string, ...any), espera func()) *Auditor {
	a := &Auditor{
		path:   path,
		logf:   logf,
		ch:     make(chan Record, auditCola),
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
		espera: espera,
	}
	go a.bucle()
	return a
}

// Record encola r sin bloquear; con la cola llena lo descarta y lo cuenta.
// Tras Close no hace nada.
func (a *Auditor) Record(r Record) {
	select {
	case <-a.quit:
		return
	default:
	}
	select {
	case a.ch <- r:
	default:
		a.dropped.Add(1)
	}
}

// Close escribe lo que quede en la cola, vacía el buffer y cierra el fichero.
// Se puede llamar más de una vez.
func (a *Auditor) Close() error {
	a.cerrar.Do(func() { close(a.quit) })
	<-a.done
	return nil
}

func (a *Auditor) bucle() {
	defer close(a.done)
	t := time.NewTicker(auditCada)
	defer t.Stop()
	for {
		select {
		case r := <-a.ch:
			a.escribir(r)
			// Con la cola vacía se vacía ya: lo escrito llega al kernel y
			// sobrevive a un SIGKILL del proceso (el daemon mata así a kling-vz
			// al congelar o parar). Solo se acumula mientras hay más esperando.
			if a.enBuffer >= auditLote || len(a.ch) == 0 {
				a.vaciar()
			}
		case <-t.C:
			a.anotarPerdidas()
			a.vaciar()
		case <-a.quit:
		resto:
			for {
				select {
				case r := <-a.ch:
					a.escribir(r)
				default:
					break resto
				}
			}
			a.anotarPerdidas()
			a.vaciar()
			a.cerrarFichero()
			return
		}
	}
}

// anotarPerdidas escribe un registro KindDropped si hay descartados que ningún
// registro ha llevado todavía y la cola está vacía (si no, los llevará el
// siguiente).
func (a *Auditor) anotarPerdidas() {
	if len(a.ch) > 0 || a.dropped.Load() == 0 {
		return
	}
	a.escribir(Record{Kind: KindDropped})
}

func (a *Auditor) escribir(r Record) {
	if a.espera != nil {
		a.espera()
	}
	if r.TS.IsZero() {
		r.TS = time.Now()
	}
	r.TS = r.TS.UTC()
	r.Dropped += a.dropped.Swap(0)
	r.Rotated += a.rotados
	a.rotados = 0
	linea, err := json.Marshal(r)
	if err != nil {
		a.perder(r.Dropped+1, err)
		return
	}
	linea = append(linea, '\n')
	if a.f == nil {
		if err := a.abrir(); err != nil {
			a.perder(r.Dropped+1, err)
			return
		}
	}
	if a.tam > 0 && a.tam+int64(len(linea)) > a.cfg.conDefectos().MaxBytes {
		// Lo que se cae de la última generación va en ESTE registro, que es
		// el primero del fichero nuevo.
		n := a.rotar()
		r.Dropped += n
		r.Rotated += n
		if linea, err = json.Marshal(r); err != nil {
			a.perder(r.Dropped+1, err)
			return
		}
		linea = append(linea, '\n')
		if err := a.abrir(); err != nil {
			a.perder(r.Dropped+1, err)
			return
		}
	}
	if _, err := a.bw.Write(linea); err != nil {
		a.rotados += r.Rotated
		a.perder(r.Dropped+1+a.enBuffer, err)
		a.enBuffer = 0
		a.cerrarFichero()
		return
	}
	a.tam += int64(len(linea))
	a.enBuffer++
}

// perder devuelve n registros a la cuenta de descartados: el siguiente que se
// escriba la llevará.
func (a *Auditor) perder(n uint64, err error) {
	a.dropped.Add(n)
	a.avisar("%v (records are being dropped and counted)", err)
}

// avisar pasa un fallo de disco a logf, como mucho uno por minuto: un disco
// lleno no debe convertirse en una línea de log por petición.
func (a *Auditor) avisar(format string, args ...any) {
	if a.logf != nil && time.Since(a.ultimoLog) > time.Minute {
		a.ultimoLog = time.Now()
		a.logf("credential audit %s: "+format, append([]any{a.path}, args...)...)
	}
}

// abrir abre (o crea) el fichero para añadir. O_NOFOLLOW y la comprobación de
// fichero regular: ver la cabecera. O_NONBLOCK solo importa si alguien planta
// una FIFO: abrirla para escribir sin lector falla en vez de colgar.
func (a *Auditor) abrir() error {
	if time.Now().Before(a.noAntesDe) {
		return errors.New("waiting to retry opening the file")
	}
	f, err := os.OpenFile(a.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		a.noAntesDe = time.Now().Add(auditReintento)
		return err
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", a.path)
	}
	if err == nil {
		// El fichero pudo existir con otros permisos: solo lo lee el dueño.
		err = f.Chmod(0o600)
	}
	if err != nil {
		_ = f.Close()
		a.noAntesDe = time.Now().Add(auditReintento)
		return err
	}
	a.f, a.bw, a.tam = f, bufio.NewWriterSize(f, 32<<10), fi.Size()
	return nil
}

// rotar desplaza las generaciones (path.N-1 a path.N, ..., path a path.1) y
// deja el siguiente para abrir. Devuelve cuántos registros se perdieron con
// la generación más antigua, que se borra: sus líneas más los descartados que
// ya llevaban anotados. Si un rename falla, se sigue en el mismo fichero:
// mejor uno que crece que perder registros.
func (a *Auditor) rotar() uint64 {
	a.vaciar()
	a.cerrarFichero()
	gens := a.cfg.conDefectos().Generations
	gen := func(i int) string { return a.path + "." + strconv.Itoa(i) }
	perdidos := contarRegistros(gen(gens))
	if err := os.Remove(gen(gens)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		a.avisar("couldn't rotate: %v", err)
		return 0
	}
	for i := gens - 1; i >= 1; i-- {
		if err := os.Rename(gen(i), gen(i+1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			a.avisar("couldn't rotate: %v", err)
		}
	}
	if err := os.Rename(a.path, gen(1)); err != nil {
		a.avisar("couldn't rotate: %v", err)
	}
	if perdidos > 0 {
		a.avisar("rotation dropped the %d oldest records (counted in dropped; raise the size or generations to keep more)", perdidos)
	}
	return perdidos
}

// contarRegistros cuenta los registros de un fichero del registro que se va a
// borrar: una por línea más los descartados que cada una llevaba. Un fichero
// que no se puede leer cuenta 0 (y no es regular: no se lee).
func contarRegistros(path string) uint64 {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return 0
	}
	var n uint64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var r struct {
			Kind    string `json:"kind"`
			Dropped uint64 `json:"dropped"`
		}
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			n += r.Dropped
			if r.Kind == KindDropped {
				continue
			}
		}
		n++
	}
	return n
}

func (a *Auditor) vaciar() {
	if a.bw == nil || a.bw.Buffered() == 0 {
		a.enBuffer = 0
		return
	}
	if err := a.bw.Flush(); err != nil {
		a.perder(a.enBuffer, err)
		a.enBuffer = 0
		a.cerrarFichero()
		return
	}
	a.enBuffer = 0
}

func (a *Auditor) cerrarFichero() {
	if a.f != nil {
		_ = a.f.Close()
	}
	a.f, a.bw, a.tam = nil, nil, 0
}
