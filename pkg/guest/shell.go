package guest

// La shell interactiva, vista desde dentro de la microVM.
//
// /exec/stream sirve para ejecutar un comando y ver su salida; esto es otra
// cosa: un pseudoterminal con una shell dentro, bytes en los dos sentidos, y
// vivo hasta que alguien cuelga. Por eso no es una respuesta en streaming sino un
// cambio de protocolo: se contesta 101, se secuestra la conexión y a partir de
// ahí viajan tramas (ver pkg/api/shell.go).
//
// Se registra en el mismo sitio que /exec, y por tanto bajo la misma puerta:
// solo existe si el kernel arrancó con kling.exec=1.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// maxShells acota las sesiones simultáneas por microVM. Cada una es una shell y
// un PTY; sin tope, quien alcance el agente puede agotar los descriptores y los
// procesos del invitado.
const maxShells = 8

// hangupGrace es lo que se espera tras colgar (cerrar el maestro, que provoca
// SIGHUP) antes de matar el grupo. Una shell bien educada sale sola en ese
// tiempo; una que ignora SIGHUP no debe poder quedarse.
const hangupGrace = 2 * time.Second

// reTerm valida el TERM que llega de fuera. Acaba en el entorno de un proceso,
// así que no se acepta cualquier cosa.
var reTerm = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type shellLimiter struct {
	mu sync.Mutex
	n  int
}

func (s *shellLimiter) take() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n >= maxShells {
		return false
	}
	s.n++
	return true
}

func (s *shellLimiter) give() {
	s.mu.Lock()
	if s.n > 0 {
		s.n--
	}
	s.mu.Unlock()
}

// ShellHandler sirve POST /exec/pty con Upgrade: kling-shell/1.
func ShellHandler(env []string) http.HandlerFunc {
	lim := &shellLimiter{}
	return func(w http.ResponseWriter, r *http.Request) { handleShell(env, lim, w, r) }
}

func handleShell(env []string, lim *shellLimiter, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), api.ShellProto) {
		http.Error(w, "this endpoint speaks "+api.ShellProto+"; ask for it with Upgrade", http.StatusUpgradeRequired)
		return
	}
	var req api.ShellRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		http.Error(w, fmt.Sprintf("invalid body: %v", err), http.StatusBadRequest)
		return
	}
	if req.Term != "" && !reTerm.MatchString(req.Term) {
		http.Error(w, fmt.Sprintf("invalid TERM %q", req.Term), http.StatusBadRequest)
		return
	}
	// Se comprueba ANTES de secuestrar: mientras la respuesta sea HTTP normal,
	// quien llama recibe un error legible.
	if err := ptySupported(); err != nil {
		http.Error(w, err.Error(), http.StatusNotImplemented)
		return
	}
	if !lim.take() {
		http.Error(w, fmt.Sprintf("too many shells open in this microVM (limit %d)", maxShells), http.StatusTooManyRequests)
		return
	}
	defer lim.give()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "this server cannot take over the connection", http.StatusInternalServerError)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close()
	// Sin plazos: una shell puede estar horas sin escribir una tecla. El hijack
	// ya los borra, pero dejarlo explícito evita que un cambio futuro en el
	// servidor los reintroduzca por la puerta de atrás.
	_ = conn.SetDeadline(time.Time{})

	if _, err := brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " +
		api.ShellProto + "\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		return
	}
	if err := brw.Flush(); err != nil {
		return
	}
	runShell(env, req, brw, conn)
}

// runShell abre el PTY, lanza la shell y hace de puente hasta que termina.
//
// in trae las tramas de quien llama (puede llevar bytes ya leídos por el
// servidor HTTP, de ahí el bufio) y out las recibe.
func runShell(env []string, req api.ShellRequest, in io.Reader, out io.Writer) {
	var wmu sync.Mutex
	escribir := func(t byte, p []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return api.WriteFrame(out, t, p)
	}

	rows, cols := req.Rows, req.Cols
	if rows == 0 || cols == 0 {
		rows, cols = 24, 80
	}
	master, slave, err := openPTY(rows, cols)
	if err != nil {
		_ = escribir(api.ShellError, []byte(err.Error()))
		return
	}
	defer master.Close()

	cmdline := req.Cmd
	if len(cmdline) == 0 {
		cmdline = api.ShellDefaultCmd
	}
	term := req.Term
	if term == "" {
		term = "xterm"
	}

	cmd := exec.Command(cmdline[0], cmdline[1:]...)
	cmd.Dir = req.Dir
	cmd.Env = append(append(append([]string(nil), env...), "TERM="+term), req.Env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	// Setsid + Setctty: la shell tiene que ser líder de su sesión y el PTY su
	// terminal de control, o no hay control de trabajos y Ctrl-C no genera
	// ninguna señal. Ctty es el descriptor EN EL HIJO: 0, que es el esclavo.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}

	exitCh, err := DefaultReaper.StartTrackedSession(cmd)
	if err != nil {
		slave.Close()
		_ = escribir(api.ShellError, []byte(fmt.Sprintf("could not start %q: %v", cmdline[0], err)))
		return
	}
	// En el padre sobra: mientras quede un descriptor del esclavo abierto aquí,
	// el maestro nunca vería EIO al morir la shell.
	slave.Close()

	// Salida del PTY hacia quien llama.
	salida := make(chan struct{})
	go func() {
		defer close(salida)
		buf := make([]byte, 32<<10)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				if werr := escribir(api.ShellData, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return // EIO: se cerró el último esclavo, o se cerró el maestro
			}
		}
	}()

	// Tramas de quien llama hacia el PTY.
	go func() {
		for {
			t, p, err := api.ReadFrame(in)
			if err != nil {
				// Colgó: cerrar el maestro manda SIGHUP a la sesión, que es lo
				// que hace un módem al colgar y lo que espera cualquier shell.
				master.Close()
				return
			}
			switch t {
			case api.ShellData:
				if _, err := master.Write(p); err != nil {
					master.Close()
					return
				}
			case api.ShellResize:
				if r, c, err := api.ParseResize(p); err == nil {
					_ = resizePTY(master, r, c)
				}
			case api.ShellSignal:
				if len(p) != 1 {
					continue
				}
				// A la sesión entera no: a quien esté en primer plano, que es lo
				// que hace una terminal de verdad.
				if pg, err := foregroundPgrp(master); err == nil && pg > 0 {
					_ = syscall.Kill(-pg, syscall.Signal(p[0]))
				}
			}
		}
	}()

	err = WaitFor(cmd, exitCh)
	if cmd.Process != nil {
		DefaultReaper.Forget(cmd.Process.Pid)
	}
	// Dar tiempo a que salga la última salida antes de anunciar el final: sin
	// esto se pierde el "logout" o el error que explica por qué murió.
	select {
	case <-salida:
	case <-time.After(200 * time.Millisecond):
	}

	code := 0
	if err != nil {
		if c, ok := ExitCodeOf(err); ok {
			code = c
		} else {
			_ = escribir(api.ShellError, []byte(err.Error()))
			return
		}
	}
	_ = escribir(api.ShellExit, api.ExitPayload(int32(code)))

	// Lo que la shell dejó vivo en segundo plano ya recibió SIGHUP al cerrar el
	// maestro. Lo que lo ignore, se va: esta microVM no debe quedarse con
	// procesos de una sesión que ya no existe.
	go func() {
		time.Sleep(hangupGrace)
		KillGroup(cmd)
	}()
}

// decodeJSON lee el cuerpo con un tope: un cuerpo sin límite es memoria del
// invitado en manos de quien llama. Un cuerpo vacío vale: significa "la shell
// por defecto, con el tamaño por defecto".
func decodeJSON(r io.Reader, v any) error {
	if err := json.NewDecoder(io.LimitReader(r, 64<<10)).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
