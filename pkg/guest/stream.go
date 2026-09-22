package guest

// Ejecución en streaming: la que usan los sandboxes.
//
// /exec (exec.go) devuelve la salida entera al final y mezclada, que es lo que
// necesita poblar un volumen. Un agente de código necesita otra cosa: ver la
// salida según sale, stdout y stderr por separado, con un plazo, con stdin, y sin
// que un comando que escupe gigas tumbe a nadie. Eso es /exec/stream.
//
// Mismas reglas que /exec: solo existe si el kernel arrancó con kling.exec=1, y
// un código de salida distinto de cero no es un error HTTP.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// chunkSize es el mayor trozo de salida por evento. Pequeño para que la salida
// llegue pronto; no tanto como para que el sobre JSON pese más que el dato.
const chunkSize = 32 << 10

// StreamExecHandler sirve POST /exec/stream: recibe un api.ExecRequest y
// responde un flujo NDJSON de api.ExecEvent. env es el entorno base de los
// comandos (el del agente, con NODE_PATH/PYTHONPATH de los volúmenes).
func StreamExecHandler(env []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { handleStreamExec(env, w, r) }
}

func handleStreamExec(env []string, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	var req api.ExecRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 2*api.ExecMaxStdin+64<<10)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid body: %v", err), http.StatusBadRequest)
		return
	}
	timeout, maxOut, err := req.Limits()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	out := newEmitter(w)

	res := runStreaming(r.Context(), env, req, timeout, maxOut, out)
	out.emit(res)
}

// runStreaming ejecuta el comando mandando su salida a out, y devuelve el evento
// final (Exit o Error).
func runStreaming(ctx context.Context, env []string, req api.ExecRequest, timeout time.Duration, maxOut int, out *emitter) api.ExecEvent {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, req.Cmd[0], req.Cmd[1:]...)
	cmd.Dir = req.Dir
	cmd.Env = append(append([]string(nil), env...), req.Env...)
	if len(req.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}
	// Al vencer el plazo o cortarse la petición se mata el GRUPO entero: un
	// `sh -c` que lanzó hijos los dejaría vivos, escribiendo en la tubería, y
	// Wait no volvería nunca.
	cmd.Cancel = func() error {
		KillGroup(cmd)
		return nil
	}
	// Y si aun así un nieto que escapó del grupo sostiene la tubería abierta, no
	// se le espera más de esto.
	cmd.WaitDelay = 2 * time.Second

	stdout := &streamWriter{out: out, stream: "stdout", left: maxOut}
	stderr := &streamWriter{out: out, stream: "stderr", left: maxOut}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	start := time.Now()
	exitCh, err := DefaultReaper.StartTracked(cmd)
	if err != nil {
		return api.ExecEvent{Error: fmt.Sprintf("could not execute %q: %v", req.Cmd[0], err)}
	}
	err = WaitFor(cmd, exitCh)
	if cmd.Process != nil {
		DefaultReaper.Forget(cmd.Process.Pid)
	}
	stdout.flush()
	stderr.flush()

	ev := api.ExecEvent{
		DurationMS: time.Since(start).Milliseconds(),
		Truncated:  stdout.truncated || stderr.truncated,
		TimedOut:   ctx.Err() == context.DeadlineExceeded,
	}
	code := 0
	if err != nil {
		c, ok := ExitCodeOf(err)
		if !ok {
			if ev.TimedOut {
				// Matado por el plazo: el código de un SIGKILL, que es lo que
				// devolvería una shell.
				c = 137
			} else {
				return api.ExecEvent{Error: fmt.Sprintf("running %q: %v", req.Cmd[0], err)}
			}
		}
		code = c
	}
	ev.Exit = &code
	return ev
}

// emitter escribe eventos NDJSON y los empuja al momento. Lo usan a la vez los
// dos flujos, de ahí el cerrojo.
type emitter struct {
	mu  sync.Mutex
	w   io.Writer
	fl  http.Flusher
	enc *json.Encoder
}

func newEmitter(w http.ResponseWriter) *emitter {
	fl, _ := w.(http.Flusher)
	return &emitter{w: w, fl: fl, enc: json.NewEncoder(w)}
}

func (e *emitter) emit(ev api.ExecEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Un error de escritura es que quien llama se fue: el comando se corta por
	// el contexto de la petición, aquí no hay nada más que hacer.
	_ = e.enc.Encode(ev)
	if e.fl != nil {
		e.fl.Flush()
	}
}

// streamWriter trocea la salida de un flujo en eventos y deja de mandar al
// llegar al tope, sin dejar de leer: cortar la tubería mataría el proceso con
// SIGPIPE, y un comando que habla mucho no tiene por qué fallar por eso.
type streamWriter struct {
	out       *emitter
	stream    string
	left      int
	truncated bool
	buf       []byte
}

func (s *streamWriter) Write(p []byte) (int, error) {
	n := len(p)
	if s.left <= 0 {
		if n > 0 {
			s.truncated = true
		}
		return n, nil
	}
	if len(p) > s.left {
		p = p[:s.left]
		s.truncated = true
	}
	s.left -= len(p)
	s.buf = append(s.buf, p...)
	for len(s.buf) >= chunkSize {
		s.send(s.buf[:chunkSize])
		s.buf = s.buf[chunkSize:]
	}
	// Lo que queda por debajo de un trozo también sale ya: la salida de un
	// comando interactivo llega a líneas sueltas y esperar a juntar 32 KiB la
	// retrasaría indefinidamente.
	if len(s.buf) > 0 {
		s.send(s.buf)
		s.buf = s.buf[:0]
	}
	return n, nil
}

func (s *streamWriter) send(b []byte) {
	s.out.emit(api.ExecEvent{Stream: s.stream, Data: append([]byte(nil), b...)})
}

func (s *streamWriter) flush() {
	if len(s.buf) > 0 {
		s.send(s.buf)
		s.buf = nil
	}
}
