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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"

	"github.com/juan52878911/kindling/pkg/api"
)

// execBootParam enciende la ruta /exec. Va en la línea de comandos del kernel,
// que solo escribe el anfitrión: el invitado no puede concedérsela a sí mismo.
const execBootParam = "kling.exec"

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

func handleExec(env []string, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
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

	cmd := exec.CommandContext(r.Context(), req.Cmd[0], req.Cmd[1:]...)
	cmd.Dir = req.Dir
	// El mismo entorno que reciben los servidores MCP, con NODE_PATH y
	// PYTHONPATH ya apuntando a los volúmenes: instalar en un volumen y luego
	// no encontrarlo desde el mismo sitio sería desconcertante.
	cmd.Env = env

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

	resp := execResponse{Output: out.String(), Truncated: out.truncated}
	if err != nil {
		if code, ok := ExitCodeOf(err); ok {
			resp.ExitCode = code
		} else {
			// Ni siquiera se pudo lanzar: no existe el binario, o no hay
			// permisos. Eso sí es un fallo de la petición, y el mensaje tiene
			// que decirlo porque no habrá salida que lo explique.
			resp.ExitCode = -1
			resp.Output += fmt.Sprintf("\ncould not execute %q: %v", req.Cmd[0], err)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// cappedBuffer junta stdout y stderr como un bytes.Buffer, pero deja de crecer
// al llegar a max. Sigue LEYENDO sin parar (nunca deja de aceptar el Write):
// cortar la tubería mataría el proceso con SIGPIPE, y un comando que habla de
// más no tiene por qué fallar por eso; el exceso simplemente no se guarda.
//
// Es seguro pasarla a la vez como cmd.Stdout y cmd.Stderr: al ser el mismo
// puntero, os/exec sirve ambos por una sola tubería y un solo escritor a la
// vez, así que no hace falta cerrojo propio.
type cappedBuffer struct {
	buf       bytes.Buffer
	left      int
	truncated bool
}

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
	c.buf.Write(p)
	return n, nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }
