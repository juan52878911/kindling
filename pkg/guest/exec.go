package guest

// Ejecutar un comando DENTRO de la microVM.
//
// Existe para una sola cosa: poblar un volumen. Instalar paquetes es ejecutar
// código de terceros, y hacerlo en el anfitrión —aunque sea en un chroot— es
// justo lo que kindling existe para evitar. Dentro de una microVM desechable, si
// un paquete trae sorpresas, se lleva por delante una máquina que se destruye
// dos segundos después.
//
// SOLO SE ACTIVA si el kernel lo pide (kling.exec=1). Una microVM de servicio no
// tiene esta ruta en absoluto: no está desactivada, no está registrada. Eso
// importa porque el gateway reenvía peticiones a los invitados, y una capacidad
// de ejecutar comandos que solo depende de no ser alcanzable es una capacidad
// que tarde o temprano alguien alcanza.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/juan52878911/kindling/pkg/api"
)

// execBootParam enciende la ruta /exec. Va en la línea de comandos del kernel,
// que solo escribe el anfitrión: el invitado no puede concedérsela a sí mismo.
const execBootParam = "kling.exec"

// execWaitDelay es lo que se espera, una vez muerto el comando (o cortada la
// petición), a que se cierren sus tuberías. Mismo valor y mismo motivo que en
// runStreaming: es una red contra nietos que sostienen la salida abierta, no un
// límite de rendimiento. Variable solo para que los tests no esperen 30 s.
var execWaitDelay = 30 * time.Second

// execMaxBody acota el cuerpo de una petición /exec legacy: solo lleva un
// comando y un directorio, nunca necesita más que esto. Sin tope, un cuerpo
// gigante se decodifica entero en memoria antes de que `Decode` llegue a fallar.
const execMaxBody = 1 << 20 // 1 MiB

type execRequest struct {
	Cmd []string `json:"cmd"`
	Dir string   `json:"dir,omitempty"`
}

type execResponse struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
	// Truncated es aditivo: un agente viejo que no lo conoce sigue leyendo
	// exit_code y output exactamente igual que antes.
	Truncated bool `json:"truncated,omitempty"`
}

// execEnabled dice si el kernel encendió la ejecución de comandos.
func ExecEnabled() bool {
	return cmdlineParams().values[execBootParam] == "1"
}

// handleExec corre un comando y devuelve su salida y su código.
//
// La salida va JUNTA (stdout y stderr entrelazados) y completa al final, no en
// streaming. Es una decisión: quien llama es el daemon poblando un volumen, no
// una persona mirando una consola, y entrelazadas se lee la causa de un fallo en
// el orden en que ocurrió. Un `npm install` que peta escribe el motivo en stderr
// entre líneas de progreso de stdout, y separarlas lo vuelve ilegible.
//
// Un código de salida distinto de cero NO es un error HTTP: la petición se
// atendió perfectamente y la respuesta es "el comando falló, aquí tienes por
// qué". Devolver 500 obligaría a quien llama a distinguir "no pude ejecutarlo"
// de "lo ejecuté y falló", que son cosas muy distintas.
//
// env es el entorno de los comandos: el mismo que reciben los servidores del
// invitado, con NODE_PATH y PYTHONPATH apuntando a los volúmenes.
func ExecHandler(env []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { handleExec(env, w, r) }
}

// ExecHandlerFunc es ExecHandler con el entorno leído en cada petición (el
// del agente cambia cuando VolumeWait monta los volúmenes).
func ExecHandlerFunc(env func() []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { handleExec(env(), w, r) }
}

// StreamExecHandlerFunc es StreamExecHandler con el entorno de cada petición.
func StreamExecHandlerFunc(env func() []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { handleStreamExec(env(), w, r) }
}

// ShellHandlerFunc es ShellHandler con el entorno de cada petición. Antes
// creaba un ShellHandler (y con él un tope nuevo) en cada petición, y
// maxShells no acotaba nada.
func ShellHandlerFunc(env func() []string) http.HandlerFunc {
	return shellHandler(env)
}

// maxExecs acota los /exec y /exec/stream en curso a la vez, entre los dos.
// Cada uno es un proceso con sus tuberías y su búfer de salida; sin tope,
// quien alcance el agente puede llevar a PID 1 al OOM en un invitado de
// 128-256 MiB. Lleno, se contesta 429 como con las shells.
const maxExecs = 64

var execLim = &slotLimiter{max: maxExecs}

// execsVivos son los /exec y /exec/stream en curso, para cortarlos todos en
// el /resync de una copia (cortarExecs).
var execsVivos = struct {
	mu  sync.Mutex
	sig uint64
	m   map[uint64]context.CancelFunc
}{m: map[uint64]context.CancelFunc{}}

// contextoExec da el contexto de un exec: el de la petición, que además corta
// cortarExecs. quitar lo da de baja; quien llama lo difiere.
func contextoExec(padre context.Context) (ctx context.Context, quitar func()) {
	ctx, cancel := context.WithCancel(padre)
	execsVivos.mu.Lock()
	execsVivos.sig++
	id := execsVivos.sig
	execsVivos.m[id] = cancel
	execsVivos.mu.Unlock()
	return ctx, func() {
		execsVivos.mu.Lock()
		delete(execsVivos.m, id)
		execsVivos.mu.Unlock()
		cancel()
	}
}

// cortarExecs cancela los exec en curso (cmd.Cancel mata su grupo) y dice
// cuántos eran. Sus plazas se sueltan cuando cada manejador acaba, no aquí.
//
// Lo llama el /resync de una copia de un dorado: un exec que corría al volcar
// despierta en cada copia, pero su petición era la de otra máquina y nadie la
// espera. Su contexto es el de una conexión congelada que la copia no cierra
// (por vsock, nunca; por TCP, cuando el keepalive se rinde), y mientras tanto
// retiene su plaza de execLim: un dorado con 64 así dejaba todas sus copias
// contestando 429.
func cortarExecs() int {
	execsVivos.mu.Lock()
	defer execsVivos.mu.Unlock()
	for _, cancel := range execsVivos.m {
		cancel()
	}
	return len(execsVivos.m)
}

// takeExec ocupa una plaza de exec o contesta 429. Si devuelve true, quien
// llama la suelta con execLim.give.
func takeExec(w http.ResponseWriter) bool {
	if execLim.take() {
		return true
	}
	execLim.mu.Lock()
	max := execLim.max
	execLim.mu.Unlock()
	http.Error(w, fmt.Sprintf("too many commands running in this microVM (limit %d)", max), http.StatusTooManyRequests)
	return false
}

func handleExec(env []string, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	if !takeExec(w) {
		return
	}
	defer execLim.give()
	r.Body = http.MaxBytesReader(w, r.Body, execMaxBody)
	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, fmt.Sprintf("request body is over the %d byte limit", execMaxBody), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, fmt.Sprintf("invalid body: %v", err), http.StatusBadRequest)
		return
	}
	if len(req.Cmd) == 0 {
		http.Error(w, "missing cmd", http.StatusBadRequest)
		return
	}

	ctx, quitar := contextoExec(r.Context())
	defer quitar()
	cmd := exec.CommandContext(ctx, req.Cmd[0], req.Cmd[1:]...)
	cmd.Dir = req.Dir
	// El mismo entorno que reciben los servidores MCP, con NODE_PATH y
	// PYTHONPATH ya apuntando a los volúmenes: instalar en un volumen y luego
	// no encontrarlo desde el mismo sitio sería desconcertante.
	cmd.Env = env
	// Igual que runStreaming: al cortarse la petición se mata el GRUPO entero
	// (StartTracked lo hace líder de su grupo), no solo al hijo. Un `sh -c` que
	// lanzó nietos los dejaba vivos tras cancelar, y con la tubería abierta.
	cmd.Cancel = func() error {
		KillGroup(cmd)
		return nil
	}
	// Y un nieto en segundo plano (`sleep 600 & echo ok`) que hereda la tubería
	// ya no retiene la respuesta hasta que acabe: pasado este plazo desde que
	// sale el comando, se cierran las tuberías y se responde con lo leído.
	cmd.WaitDelay = execWaitDelay

	// Por el cosechador, igual que los servidores MCP: si se adelanta a nuestro
	// Wait, CombinedOutput devolvería un error que no es *ExitError y un
	// `npm install` que terminó bien se reportaría como "no pude ejecutarlo".
	//
	// out tiene tope (api.ExecMaxOutput): sin él, un comando que escupe GiBs
	// (logs verbosos de npm, un script fuera de control) hincha el buffer hasta
	// que el OOM killer se lleva por delante PID 1, que tumba la máquina entera.
	// Sigue leyendo sin parar tras el tope, para no cortar la tubería con SIGPIPE.
	out := newCappedBuffer(api.ExecMaxOutput)
	cmd.Stdout, cmd.Stderr = out, out
	exitCh, err := DefaultReaper.StartTracked(cmd)
	if err != nil {
		resp := execResponse{ExitCode: -1,
			Output: fmt.Sprintf("could not execute %q: %v", req.Cmd[0], err)}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	err = WaitFor(cmd, exitCh)
	if cmd.Process != nil {
		DefaultReaper.Forget(cmd.Process.Pid)
	}

	code := 0
	var cola string
	if err != nil {
		var ok bool
		code, ok = ExitCodeOf(err)
		// El proceso pudo terminar y fallar solo el ESPERARLO (WaitDelay agotado
		// por un nieto que sostenía la tubería): si hay estado de salida, ese es
		// el resultado, no "no pude ejecutarlo".
		if !ok && cmd.ProcessState != nil {
			code, ok = ExitCodeOf(&exec.ExitError{ProcessState: cmd.ProcessState})
		}
		if !ok {
			// Ni siquiera se pudo lanzar: no existe el binario, o no hay
			// permisos. Eso sí es un fallo de la petición, y el mensaje tiene
			// que decirlo porque no habrá salida que lo explique.
			code = -1
			cola = fmt.Sprintf("\ncould not execute %q: %v", req.Cmd[0], err)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = writeExecResponse(w, code, out, cola)
	// Tras una salida grande, la memoria vuelve al sistema ya y no cuando le
	// toque al recolector: en un invitado pequeño son MiB que el globo puede
	// reclamar. Con una pequeña no compensa una GC entera.
	if out.size >= execLiberarDesde {
		out.reset()
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		liberarMemoria()
	}
}

// execLiberarDesde es la salida a partir de la cual /exec devuelve la memoria
// al sistema al acabar.
const execLiberarDesde = 4 << 20

// liberarMemoria es debug.FreeOSMemory; variable para los tests.
var liberarMemoria = debug.FreeOSMemory

// writeExecResponse escribe la misma respuesta que json.Encoder sobre
// execResponse, pero sin construirla en memoria: la salida va de los bloques
// de out a w escapada por trozos. Antes out.String() la copiaba entera y
// json la volvía a copiar escapada, y el pico eran 3-4× la salida. Los
// campos salen en otro orden (exit_code primero, como antes), y quien
// decodifica no lo nota.
func writeExecResponse(w io.Writer, code int, out *cappedBuffer, cola string) error {
	bw := bufio.NewWriterSize(w, 32<<10)
	fmt.Fprintf(bw, `{"exit_code":%d,"output":`, code)
	if cola != "" {
		// La cola va dentro del mismo string: se escribe como un bloque más.
		out.appendRaw([]byte(cola))
	}
	if err := out.writeJSONString(bw); err != nil {
		return err
	}
	if out.truncated {
		bw.WriteString(`,"truncated":true`)
	}
	bw.WriteString("}\n")
	return bw.Flush()
}

// cappedBuffer junta stdout y stderr como un bytes.Buffer, pero deja de crecer
// al llegar a max. Sigue LEYENDO sin parar (nunca deja de aceptar el Write):
// cortar la tubería mataría el proceso con SIGPIPE, y un comando que habla de
// más no tiene por qué fallar por eso; el exceso simplemente no se guarda.
//
// Guarda en bloques y no en un solo slice: un bytes.Buffer que crece doblando
// copia lo que lleva en cada paso y llega a tener reservado el doble de la
// salida. Los bloques empiezan en 4 KiB (un `echo ok` no reserva más) y
// doblan hasta 256 KiB.
//
// Es seguro pasarla a la vez como cmd.Stdout y cmd.Stderr: al ser el mismo
// puntero, os/exec sirve ambos por una sola tubería y un solo escritor a la
// vez, así que no hace falta cerrojo propio.
type cappedBuffer struct {
	bloques   [][]byte
	size      int
	left      int
	truncated bool
}

const (
	cappedBloqueMin = 4 << 10
	cappedBloqueMax = 256 << 10
)

func newCappedBuffer(max int) *cappedBuffer {
	return &cappedBuffer{left: max}
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if c.left <= 0 {
		if n > 0 {
			c.truncated = true
		}
		return n, nil
	}
	if len(p) > c.left {
		p = p[:c.left]
		c.truncated = true
	}
	c.left -= len(p)
	c.appendRaw(p)
	return n, nil
}

// appendRaw añade p sin mirar el tope.
func (c *cappedBuffer) appendRaw(p []byte) {
	c.size += len(p)
	for len(p) > 0 {
		if k := len(c.bloques); k == 0 || len(c.bloques[k-1]) == cap(c.bloques[k-1]) {
			tam := cappedBloqueMin
			if k > 0 {
				tam = min(2*cap(c.bloques[k-1]), cappedBloqueMax)
			}
			c.bloques = append(c.bloques, make([]byte, 0, tam))
		}
		b := &c.bloques[len(c.bloques)-1]
		n := min(len(p), cap(*b)-len(*b))
		*b = append(*b, p[:n]...)
		p = p[n:]
	}
}

// reset suelta los bloques.
func (c *cappedBuffer) reset() { c.bloques, c.size = nil, 0 }

func (c *cappedBuffer) String() string {
	var sb strings.Builder
	sb.Grow(c.size)
	for _, b := range c.bloques {
		sb.Write(b)
	}
	return sb.String()
}

// writeJSONString escribe lo guardado como un string de JSON, con las mismas
// reglas que encoding/json: <, > y & escapados, U+2028 y U+2029 también, y
// cada byte de UTF-8 inválido como U+FFFD. Una runa partida entre dos bloques
// se junta antes de decodificarla.
func (c *cappedBuffer) writeJSONString(w *bufio.Writer) error {
	w.WriteByte('"')
	var resto [utf8.UTFMax]byte // el principio de una runa que sigue en el bloque siguiente
	nr := 0
	for i, b := range c.bloques {
		ultimo := i == len(c.bloques)-1
		if nr > 0 {
			// Completa la runa partida con lo justo del bloque nuevo.
			k := min(len(b), utf8.UTFMax-nr)
			var junta [2 * utf8.UTFMax]byte
			n := copy(junta[:], resto[:nr])
			n += copy(junta[n:], b[:k])
			_, tam := utf8.DecodeRune(junta[:n])
			if tam <= nr {
				// Lo de antes era inválido por sí solo: un U+FFFD por byte, y el
				// bloque nuevo empieza limpio.
				escaparJSON(w, resto[:nr])
				nr = 0
			} else {
				escaparJSON(w, junta[:tam])
				b = b[tam-nr:]
				nr = 0
			}
		}
		if !ultimo {
			// Lo que al final del bloque es el principio de una runa sin
			// acabar se queda para el siguiente.
			if cola := runaIncompleta(b); cola > 0 {
				nr = copy(resto[:], b[len(b)-cola:])
				b = b[:len(b)-cola]
			}
		}
		escaparJSON(w, b)
	}
	if nr > 0 {
		escaparJSON(w, resto[:nr])
	}
	w.WriteByte('"')
	return nil
}

// runaIncompleta dice cuántos bytes del final de b son el principio de una
// runa de UTF-8 que no acaba en b (0 si no hay).
func runaIncompleta(b []byte) int {
	for i := 1; i < utf8.UTFMax && i <= len(b); i++ {
		c := b[len(b)-i]
		if c < utf8.RuneSelf {
			return 0
		}
		if utf8.RuneStart(c) {
			if utf8.FullRune(b[len(b)-i:]) {
				return 0
			}
			return i
		}
	}
	return 0
}

const hexJSON = "0123456789abcdef"

// escaparJSON escribe b escapado como dentro de un string de JSON.
func escaparJSON(w *bufio.Writer, b []byte) {
	ini := 0
	for i := 0; i < len(b); {
		if c := b[i]; c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' && c != '<' && c != '>' && c != '&' {
				i++
				continue
			}
			w.Write(b[ini:i])
			switch c {
			case '"', '\\':
				w.WriteByte('\\')
				w.WriteByte(c)
			case '\n':
				w.WriteString(`\n`)
			case '\r':
				w.WriteString(`\r`)
			case '\t':
				w.WriteString(`\t`)
			default:
				w.WriteString(`\u00`)
				w.WriteByte(hexJSON[c>>4])
				w.WriteByte(hexJSON[c&0xf])
			}
			i++
			ini = i
			continue
		}
		r, tam := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && tam == 1 {
			w.Write(b[ini:i])
			w.WriteString(`\ufffd`)
			i++
			ini = i
			continue
		}
		if r == '\u2028' || r == '\u2029' {
			w.Write(b[ini:i])
			w.WriteString(`\u202`)
			w.WriteByte(hexJSON[r&0xf])
			i += tam
			ini = i
			continue
		}
		i += tam
	}
	w.Write(b[ini:])
}
