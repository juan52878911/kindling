package guest

// Procesos en segundo plano: lo que sigue corriendo cuando quien lo lanzó se va.
//
// /exec y /exec/stream atan el comando a la conexión: si se cierra, se mata el
// grupo. Aquí no: el proceso lo posee el agente, que guarda su estado y su
// salida (un anillo acotado por proceso) en una tabla acotada. El daemon los
// expone en /machines/{ref}/processes.
//
// La tabla vive en memoria del agente, así que sigue ahí tras congelar y
// descongelar la máquina (la memoria se conserva) y se pierde con un reinicio.
// El resync de una COPIA de un snapshot mata lo que corría al volcar: los
// procesos eran de otra máquina (igual que cortarExecs con los exec).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/juan52878911/kindling/pkg/api"
)

// procChunk es un trozo de salida: off es su desplazamiento de inicio.
type procChunk struct {
	off    int64
	stream string
	data   []byte
}

type proceso struct {
	cmd *exec.Cmd

	mu       sync.Mutex
	changed  chan struct{} // se cierra y se repone en cada escritura o salida
	info     api.ProcessInfo
	ring     []procChunk
	ringSize int
	ringCap  int
	total    int64 // desplazamiento tras el último byte
	done     bool
	timer    *time.Timer
}

func (p *proceso) notificar() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// escribir añade salida al anillo, descartando lo más viejo si se pasa.
func (p *proceso) escribir(stream string, b []byte) {
	if len(b) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(b) > 0 {
		n := len(b)
		if n > chunkSize {
			n = chunkSize
		}
		p.ring = append(p.ring, procChunk{off: p.total, stream: stream, data: append([]byte(nil), b[:n]...)})
		p.total += int64(n)
		p.ringSize += n
		b = b[n:]
	}
	// Pasado el tope se recorta por la cabeza, aun a mitad de un trozo: así
	// siempre se conservan ringCap bytes, no lo que quepa en trozos enteros.
	for exceso := p.ringSize - p.ringCap; exceso > 0; exceso = p.ringSize - p.ringCap {
		c := p.ring[0]
		if len(c.data) <= exceso {
			p.ringSize -= len(c.data)
			p.ring[0] = procChunk{}
			p.ring = p.ring[1:]
			continue
		}
		p.ring[0] = procChunk{off: c.off + int64(exceso), stream: c.stream, data: c.data[exceso:]}
		p.ringSize -= exceso
	}
	p.info.OutputBytes = p.total
	p.info.OutputStart = p.inicioLocked()
	p.notificar()
}

func (p *proceso) inicioLocked() int64 {
	if len(p.ring) == 0 {
		return p.total
	}
	return p.ring[0].off
}

// leer devuelve los trozos desde offset, el desplazamiento siguiente, lo que
// se perdió por estar ya fuera del anillo y si el proceso ya acabó (y no queda
// nada más que leer).
func (p *proceso) leer(offset int64) (chunks []procChunk, next, dropped int64, fin bool, ch <-chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if start := p.inicioLocked(); offset < start {
		dropped = start - offset
		offset = start
	}
	if offset > p.total {
		offset = p.total
	}
	for _, c := range p.ring {
		end := c.off + int64(len(c.data))
		if end <= offset {
			continue
		}
		if c.off < offset {
			c.data = c.data[offset-c.off:]
			c.off = offset
		}
		chunks = append(chunks, c)
	}
	return chunks, p.total, dropped, p.done, p.changed
}

func (p *proceso) snapshot() api.ProcessInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	in := p.info
	in.OutputStart = p.inicioLocked()
	return in
}

type procWriter struct {
	p      *proceso
	stream string
}

func (w procWriter) Write(b []byte) (int, error) {
	w.p.escribir(w.stream, b)
	return len(b), nil
}

var procs = struct {
	mu sync.Mutex
	m  map[int]*proceso
}{m: map[int]*proceso{}}

var nombresSenal = map[string]syscall.Signal{
	"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT, "KILL": syscall.SIGKILL,
	"USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2, "TERM": syscall.SIGTERM, "CONT": syscall.SIGCONT,
	"STOP": syscall.SIGSTOP, "TSTP": syscall.SIGTSTP, "ALRM": syscall.SIGALRM, "PIPE": syscall.SIGPIPE,
	"ABRT": syscall.SIGABRT,
}

func parseSenal(s string) (syscall.Signal, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return syscall.SIGTERM, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n < 1 || n > 64 {
			return 0, fmt.Errorf("signal %d out of range", n)
		}
		return syscall.Signal(n), nil
	}
	if sig, ok := nombresSenal[strings.TrimPrefix(s, "SIG")]; ok {
		return sig, nil
	}
	return 0, fmt.Errorf("unknown signal %q", s)
}

func nombreSenal(sig syscall.Signal) string {
	for n, s := range nombresSenal {
		if s == sig {
			return n
		}
	}
	return "SIG" + strconv.Itoa(int(sig))
}

// senalDe dice qué señal mató al proceso, si lo hizo una.
func senalDe(err error) (syscall.Signal, bool) {
	var re *reapedExit
	if errors.As(err, &re) && re.status.Signaled() {
		return re.status.Signal(), true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return ws.Signal(), true
		}
	}
	return 0, false
}

func httpErr(w http.ResponseWriter, code int, format string, a ...any) {
	http.Error(w, fmt.Sprintf(format, a...), code)
}

// podarProcs hace sitio para uno más: si la tabla está llena olvida el
// terminado más antiguo. false = todo lo que hay sigue vivo. Con procs.mu.
func podarProcsLocked() bool {
	if len(procs.m) < api.ProcessMaxEntries {
		return true
	}
	var viejo *proceso
	for _, p := range procs.m {
		p.mu.Lock()
		hecho, fin := p.done, p.info.EndedAt
		p.mu.Unlock()
		if !hecho {
			continue
		}
		if viejo == nil || fin.Before(*viejo.info.EndedAt) {
			viejo = p
		}
	}
	if viejo == nil {
		return false
	}
	delete(procs.m, viejo.info.PID)
	return true
}

func vivos() int {
	n := 0
	for _, p := range procs.m {
		p.mu.Lock()
		if !p.done {
			n++
		}
		p.mu.Unlock()
	}
	return n
}

// ProcessesHandlerFunc sirve /processes y lo que cuelga de él.
func ProcessesHandlerFunc(env func() []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.Trim(strings.TrimPrefix(r.URL.Path, api.GuestProcessesPath), "/")
		if rest == "" {
			switch r.Method {
			case http.MethodPost:
				iniciarProceso(env(), w, r)
			case http.MethodGet:
				listarProcesos(w)
			default:
				httpErr(w, http.StatusMethodNotAllowed, "method not allowed")
			}
			return
		}
		pidS, sub, _ := strings.Cut(rest, "/")
		pid, err := strconv.Atoi(pidS)
		if err != nil || pid <= 0 {
			httpErr(w, http.StatusBadRequest, "invalid pid %q", pidS)
			return
		}
		procs.mu.Lock()
		p := procs.m[pid]
		procs.mu.Unlock()
		if p == nil {
			httpErr(w, http.StatusNotFound, "no background process with pid %d", pid)
			return
		}
		switch {
		case sub == "" && r.Method == http.MethodGet:
			estadoProceso(w, r, p)
		case sub == "" && r.Method == http.MethodDelete:
			borrarProceso(w, p)
		case sub == "output" && r.Method == http.MethodGet:
			salidaProceso(w, r, p)
		case sub == "signal" && r.Method == http.MethodPost:
			senalarProceso(w, r, p)
		default:
			httpErr(w, http.StatusNotFound, "no such route")
		}
	}
}

func iniciarProceso(env []string, w http.ResponseWriter, r *http.Request) {
	var req api.ProcessStartRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, execMaxBody)).Decode(&req); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	if len(req.Cmd) == 0 || req.Cmd[0] == "" {
		httpErr(w, http.StatusBadRequest, "missing cmd")
		return
	}
	if req.TimeoutSeconds < 0 {
		httpErr(w, http.StatusBadRequest, "timeout_seconds can't be negative")
		return
	}
	ringCap := api.ProcessDefaultOutput
	if req.MaxOutputBytes < 0 || req.MaxOutputBytes > api.ProcessMaxOutput {
		httpErr(w, http.StatusBadRequest, "max_output_bytes must be between 0 and %d", api.ProcessMaxOutput)
		return
	}
	if req.MaxOutputBytes > 0 {
		ringCap = req.MaxOutputBytes
	}

	dir, baseEnv := req.Dir, env
	if req.Session != "" {
		ses, ok := usarSesion(req.Session)
		if !ok {
			httpErr(w, http.StatusNotFound, "no session %q (expired or never created)", req.Session)
			return
		}
		baseEnv = ses.entornoDe(env)
		if dir == "" {
			dir = ses.dir
		}
	}
	cmd := exec.Command(req.Cmd[0], req.Cmd[1:]...)
	cmd.Dir = dir
	cmd.Env = append(append([]string(nil), baseEnv...), req.Env...)
	if req.User != "" {
		cred, home, err := credencialExec(req.User)
		if err != nil {
			httpErr(w, http.StatusBadRequest, "user %q: %v", req.User, err)
			return
		}
		if cred != nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
			if home != "" {
				cmd.Env = append(setEnv(append([]string(nil), baseEnv...), "HOME", home), req.Env...)
			}
		}
	}

	p := &proceso{cmd: cmd, changed: make(chan struct{}), ringCap: ringCap}
	cmd.Stdout, cmd.Stderr = procWriter{p, "stdout"}, procWriter{p, "stderr"}
	// Sin WaitDelay no: un nieto que retenga la tubería no debe cerrar la
	// salida de un proceso que sigue vivo; WaitDelay solo corre tras salir el
	// hijo, así que este plazo es el de vaciar tuberías de un proceso ya muerto.
	cmd.WaitDelay = 2 * time.Second

	// El cerrojo de la tabla cubre Start y el alta: un hijo que muere al
	// instante no se pierde, y el límite no se rebasa con dos peticiones.
	procs.mu.Lock()
	if vivos() >= api.ProcessMaxRunning {
		procs.mu.Unlock()
		w.Header().Set("Retry-After", "1")
		httpErr(w, http.StatusTooManyRequests, "too many background processes running in this microVM (limit %d)", api.ProcessMaxRunning)
		return
	}
	if !podarProcsLocked() {
		procs.mu.Unlock()
		httpErr(w, http.StatusTooManyRequests, "too many background processes in this microVM (limit %d)", api.ProcessMaxEntries)
		return
	}
	// La ficha, antes de Start: la salida puede llegar antes de que volvamos.
	p.info = api.ProcessInfo{Cmd: req.Cmd, Dir: dir, State: api.ProcessRunning, StartedAt: time.Now()}
	exitCh, err := DefaultReaper.StartTracked(cmd)
	if err != nil {
		procs.mu.Unlock()
		httpErr(w, http.StatusBadRequest, "could not execute %q: %v", req.Cmd[0], err)
		return
	}
	pid := cmd.Process.Pid
	p.mu.Lock()
	p.info.PID = pid
	p.mu.Unlock()
	procs.m[pid] = p
	procs.mu.Unlock()

	if req.TimeoutSeconds > 0 {
		p.mu.Lock()
		p.timer = time.AfterFunc(time.Duration(req.TimeoutSeconds)*time.Second, func() {
			p.mu.Lock()
			if !p.done {
				p.info.TimedOut = true
			}
			p.mu.Unlock()
			KillGroup(cmd)
		})
		p.mu.Unlock()
	}
	go esperarProceso(p, exitCh)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(p.snapshot())
}

func esperarProceso(p *proceso, exitCh chan syscall.WaitStatus) {
	err := WaitFor(p.cmd, exitCh)
	DefaultReaper.Forget(p.cmd.Process.Pid)
	code := 0
	if err != nil {
		c, ok := ExitCodeOf(err)
		if !ok && p.cmd.ProcessState != nil {
			c, ok = p.cmd.ProcessState.ExitCode(), true
		}
		if !ok {
			c = -1
		}
		code = c
	}
	sig, signaled := senalDe(err)
	now := time.Now()
	p.mu.Lock()
	if p.timer != nil {
		p.timer.Stop()
	}
	p.done = true
	p.info.State = api.ProcessExited
	p.info.EndedAt = &now
	p.info.ExitCode = &code
	if signaled {
		p.info.Signal = nombreSenal(sig)
	}
	p.notificar()
	p.mu.Unlock()
}

// maxCmdLista es lo que la lista de procesos trae de cada Cmd; el detalle de
// un proceso lo trae entero.
const maxCmdLista = 4 << 10

// recortarCmd devuelve cmd con, como mucho, max bytes en total (el último
// argumento se corta y los siguientes se omiten). No toca el original.
func recortarCmd(cmd []string, max int) []string {
	n := 0
	for _, a := range cmd {
		n += len(a)
	}
	if n <= max {
		return cmd
	}
	out := make([]string, 0, len(cmd))
	resto := max
	for _, a := range cmd {
		if resto <= 0 {
			break
		}
		if len(a) > resto {
			a = a[:resto]
			for len(a) > 0 && !utf8.ValidString(a) {
				a = a[:len(a)-1]
			}
		}
		out = append(out, a)
		resto -= len(a)
	}
	return out
}

func listarProcesos(w http.ResponseWriter) {
	procs.mu.Lock()
	all := make([]*proceso, 0, len(procs.m))
	for _, p := range procs.m {
		all = append(all, p)
	}
	procs.mu.Unlock()
	out := make([]api.ProcessInfo, 0, len(all))
	for _, p := range all {
		i := p.snapshot()
		i.Cmd = recortarCmd(i.Cmd, maxCmdLista)
		out = append(out, i)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}
		return out[i].PID < out[j].PID
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// maxEsperaProceso acota ?wait=: el daemon repite si hace falta.
const maxEsperaProceso = 10 * time.Minute

func estadoProceso(w http.ResponseWriter, r *http.Request, p *proceso) {
	if ws := r.URL.Query().Get("wait"); ws != "" {
		n, err := strconv.Atoi(ws)
		if err != nil || n < 0 {
			httpErr(w, http.StatusBadRequest, "invalid wait %q", ws)
			return
		}
		d := time.Duration(n) * time.Second
		if d > maxEsperaProceso {
			d = maxEsperaProceso
		}
		timer := time.NewTimer(d)
		defer timer.Stop()
		for {
			p.mu.Lock()
			done, ch := p.done, p.changed
			p.mu.Unlock()
			if done {
				break
			}
			select {
			case <-ch:
			case <-timer.C:
				goto respuesta
			case <-r.Context().Done():
				return
			}
		}
	}
respuesta:
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(p.snapshot())
}

func borrarProceso(w http.ResponseWriter, p *proceso) {
	p.mu.Lock()
	hecho := p.done
	p.mu.Unlock()
	if !hecho {
		KillGroup(p.cmd)
	}
	procs.mu.Lock()
	delete(procs.m, p.info.PID)
	procs.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func senalarProceso(w http.ResponseWriter, r *http.Request, p *proceso) {
	var req api.ProcessSignalRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		httpErr(w, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	sig, err := parseSenal(req.Signal)
	if err != nil {
		httpErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	p.mu.Lock()
	hecho := p.done
	p.mu.Unlock()
	if hecho {
		httpErr(w, http.StatusConflict, "process %d already exited", p.info.PID)
		return
	}
	pid := p.cmd.Process.Pid
	if pid <= 1 {
		httpErr(w, http.StatusBadRequest, "refusing to signal pid %d", pid)
		return
	}
	// Al grupo, con lo que haya lanzado; si no tiene grupo propio, al pid.
	if err := syscall.Kill(-pid, sig); err != nil {
		if err2 := syscall.Kill(pid, sig); err2 != nil {
			httpErr(w, http.StatusConflict, "signalling %d: %v", pid, err2)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(p.snapshot())
}

// latidoSalida es cada cuánto un ?follow=1 sin salida manda un evento vacío,
// para que ni el daemon ni un proxy intermedio den el flujo por colgado.
var latidoSalida = 10 * time.Second

func salidaProceso(w http.ResponseWriter, r *http.Request, p *proceso) {
	offset := int64(0)
	if s := r.URL.Query().Get("offset"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			httpErr(w, http.StatusBadRequest, "invalid offset %q", s)
			return
		}
		offset = n
	}
	follow := r.URL.Query().Get("follow") == "1"
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	emit := func(ev api.ProcessEvent) bool {
		buf.Reset()
		_ = enc.Encode(ev)
		if _, err := w.Write(buf.Bytes()); err != nil {
			return false
		}
		if fl != nil {
			fl.Flush()
		}
		return true
	}
	beat := time.NewTicker(latidoSalida)
	defer beat.Stop()
	for {
		chunks, next, dropped, fin, ch := p.leer(offset)
		for i, c := range chunks {
			ev := api.ProcessEvent{Stream: c.stream, Data: c.data, Offset: c.off + int64(len(c.data))}
			if i == 0 {
				ev.Dropped = dropped
			}
			if !emit(ev) {
				return
			}
		}
		if len(chunks) == 0 && dropped > 0 {
			if !emit(api.ProcessEvent{Offset: next, Dropped: dropped}) {
				return
			}
		}
		offset = next
		if fin || !follow {
			if fin {
				in := p.snapshot()
				ev := api.ProcessEvent{Offset: offset, Exit: in.ExitCode, Signal: in.Signal}
				_ = emit(ev)
			}
			return
		}
		select {
		case <-ch:
		case <-beat.C:
			if !emit(api.ProcessEvent{Offset: offset}) {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// limpiarProcesos mata y olvida todos los procesos de fondo y las sesiones:
// lo que hace el resync de una copia de un dorado, cuyos procesos eran de
// otra máquina. Devuelve cuántos procesos estaban vivos.
func limpiarProcesos() int {
	procs.mu.Lock()
	all := procs.m
	procs.m = map[int]*proceso{}
	procs.mu.Unlock()
	n := 0
	for _, p := range all {
		p.mu.Lock()
		hecho := p.done
		p.mu.Unlock()
		if !hecho {
			n++
			KillGroup(p.cmd)
		}
	}
	olvidarSesiones()
	return n
}
