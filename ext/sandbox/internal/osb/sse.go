package osb

// Flujos SSE de execd. El execd de OpenSandbox emite un ServerStreamEvent por
// evento; aquí van como SSE estándar (`data: <json>\n\n`), que el SDK acepta igual
// que las tramas JSON sueltas de las versiones viejas de execd.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

// evento es el ServerStreamEvent de la spec de execd.
type evento struct {
	Type           string         `json:"type"`
	Text           string         `json:"text,omitempty"`
	ExecutionCount int            `json:"execution_count,omitempty"`
	ExitCode       *int           `json:"exit_code,omitempty"`
	ExecutionTime  int64          `json:"execution_time,omitempty"`
	Timestamp      int64          `json:"timestamp"`
	Results        map[string]any `json:"results,omitempty"`
	Error          *errorEvento   `json:"error,omitempty"`
}

type errorEvento struct {
	EName     string   `json:"ename"`
	EValue    string   `json:"evalue"`
	Traceback []string `json:"traceback"`
	ExitCode  *int     `json:"exit_code,omitempty"`
}

// latidoSSE es cada cuánto se manda un evento ping en un flujo callado (el
// execd real usa 3 s): así un proxy no cierra un comando que no escribe nada.
var latidoSSE = 3 * time.Second

// flujoSSE escribe eventos de uno en uno y vacía cada uno al momento. Es seguro
// entre goroutines (el latido escribe en paralelo).
type flujoSSE struct {
	mu  sync.Mutex
	w   http.ResponseWriter
	fl  http.Flusher
	fin chan struct{}
	wg  sync.WaitGroup
}

// nuevoSSE fija las cabeceras, manda el 200 y arranca el latido. cerrar() lo
// para; hay que llamarlo antes de que vuelva el manejador.
func nuevoSSE(w http.ResponseWriter) *flujoSSE {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f := &flujoSSE{w: w, fin: make(chan struct{})}
	f.fl, _ = w.(http.Flusher)
	if f.fl != nil {
		f.fl.Flush()
	}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		t := time.NewTicker(latidoSSE)
		defer t.Stop()
		for {
			select {
			case <-f.fin:
				return
			case <-t.C:
				f.enviar(evento{Type: "ping"})
			}
		}
	}()
	return f
}

func (f *flujoSSE) cerrar() {
	close(f.fin)
	f.wg.Wait()
}

func ahoraMS() int64 { return time.Now().UnixMilli() }

func (f *flujoSSE) enviar(e evento) {
	if e.Timestamp == 0 {
		e.Timestamp = ahoraMS()
	}
	b, _ := json.Marshal(e)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.w.Write([]byte("data: "))
	f.w.Write(b)
	f.w.Write([]byte("\n\n"))
	if f.fl != nil {
		f.fl.Flush()
	}
}

func (f *flujoSSE) fallo(nombre, msg string) {
	f.enviar(evento{Type: "error", Error: &errorEvento{EName: nombre, EValue: msg, Traceback: []string{msg}}})
}

// textoUTF8 parte la salida en texto sin cortar runas: lo que queda a medias de
// un trozo se guarda para el siguiente.
type textoUTF8 struct{ resto []byte }

func (t *textoUTF8) trozo(b []byte) string {
	b = append(t.resto, b...)
	t.resto = nil
	corte := len(b)
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				corte = i
			}
			break
		}
	}
	t.resto = append([]byte(nil), b[corte:]...)
	return string(b[:corte])
}

func (t *textoUTF8) vaciar() string {
	s := string(t.resto)
	t.resto = nil
	return s
}

// eventoExec es una línea del NDJSON de exec del gateway (api.ExecEvent).
type eventoExec struct {
	Stream     string `json:"stream"`
	Data       []byte `json:"data"`
	Exit       *int   `json:"exit"`
	DurationMS int64  `json:"duration_ms"`
	TimedOut   bool   `json:"timed_out"`
	Error      string `json:"error"`
}

// traducirExec convierte el NDJSON de exec del gateway en eventos de execd, uno
// por línea y en el momento: stdout/stderr tal cual (en texto), y al final
// execution_complete (salida 0) o error con exit_code (salida distinta de 0),
// como el execd real. eco recibe cada trozo de salida (para los logs del
// comando); devuelve el código de salida (nil si el flujo se cortó).
func traducirExec(ctx context.Context, f *flujoSSE, cuerpo io.Reader, eco func([]byte)) *int {
	sc := bufio.NewScanner(cuerpo)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var out, errT textoUTF8
	emitir := func(tipo, s string) {
		if s != "" {
			f.enviar(evento{Type: tipo, Text: s})
		}
	}
	for sc.Scan() {
		var ev eventoExec
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch {
		case ev.Error != "":
			emitir("stdout", out.vaciar())
			emitir("stderr", errT.vaciar())
			f.fallo("RuntimeError", ev.Error)
			return nil
		case ev.Exit != nil:
			emitir("stdout", out.vaciar())
			emitir("stderr", errT.vaciar())
			code := *ev.Exit
			if code == 0 {
				f.enviar(evento{Type: "execution_complete", ExecutionTime: ev.DurationMS, ExitCode: &code})
			} else {
				msg := "exit status " + strconv.Itoa(code)
				if ev.TimedOut {
					msg = "command timed out"
				}
				f.enviar(evento{Type: "error", Error: &errorEvento{EName: "CommandExecError",
					EValue: strconv.Itoa(code), Traceback: []string{msg}, ExitCode: &code}})
			}
			return &code
		case ev.Stream == "stdout":
			if eco != nil {
				eco(ev.Data)
			}
			emitir("stdout", out.trozo(ev.Data))
		case ev.Stream == "stderr":
			if eco != nil {
				eco(ev.Data)
			}
			emitir("stderr", errT.trozo(ev.Data))
		}
	}
	emitir("stdout", out.vaciar())
	emitir("stderr", errT.vaciar())
	if ctx.Err() != nil {
		f.fallo("CommandInterrupted", "command interrupted")
	} else {
		f.fallo("RuntimeError", "the gateway closed the stream before the command finished")
	}
	return nil
}
