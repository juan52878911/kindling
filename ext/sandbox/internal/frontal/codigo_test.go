package frontal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// evCodigo es una línea del flujo NDJSON de POST .../code, leída de forma
// laxa (el test no depende de los tipos del paquete).
type evCodigo struct {
	Type      string   `json:"type"`
	Text      string   `json:"text"`
	Ename     string   `json:"ename"`
	Evalue    string   `json:"evalue"`
	Traceback []string `json:"traceback"`
	OK        *bool    `json:"ok"`
}

// daemonReal pone delante del daemon falso uno que SÍ ejecuta: exec, procesos
// de fondo y ficheros corren en esta máquina (los contextos usan rutas bajo
// /tmp con un id aleatorio, así que no chocan). Sirve para probar el gateway y
// el conductor de punta a punta sin KVM. Si sinPython, el exec que busca
// python3 contesta 127 como un invitado sin él.
type daemonReal struct {
	mu        sync.Mutex
	procs     map[int]chan struct{} // se cierra cuando el proceso termina
	info      map[int]api.ProcessInfo
	sinPython bool
	execs     []api.ExecRequest
	// corta hace que el exec "run" mande un evento y corte la conexión.
	corta bool
}

func (d *daemonReal) vivo(pid int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.procs[pid]
	if c == nil {
		return false
	}
	select {
	case <-c:
		return false
	default:
		return true
	}
}

func ponerReal(t *testing.T, f *falso) *daemonReal {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 in this runner")
	}
	dir, err := os.MkdirTemp("", "dr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no unix sockets here: %v", err)
	}
	d := &daemonReal{procs: map[int]chan struct{}{}, info: map[int]api.ProcessInfo{}}
	detras := f.endpoint
	proxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) { r.URL.Scheme, r.URL.Host = "http", "daemon" },
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", detras)
		}},
	}
	m := http.NewServeMux()
	m.HandleFunc("POST /machines/{ref}/exec", func(w http.ResponseWriter, r *http.Request) {
		if !f.existe(r.PathValue("ref")) {
			failDaemon(w, 404, os.ErrNotExist)
			return
		}
		var req api.ExecRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.mu.Lock()
		d.execs = append(d.execs, req)
		sinPy := d.sinPython
		corta := d.corta && len(req.Cmd) > 2 && req.Cmd[2] == "run"
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		enc := json.NewEncoder(w)
		fl := w.(http.Flusher)
		if corta {
			_ = enc.Encode(api.ExecEvent{Stream: "stdout", Data: []byte("{\"type\":\"stdout\",\"text\":\"x\"}\n")})
			fl.Flush()
			panic(http.ErrAbortHandler)
		}
		if sinPy && len(req.Cmd) > 2 && strings.Contains(req.Cmd[2], "python3") {
			code := 127
			_ = enc.Encode(api.ExecEvent{Exit: &code})
			return
		}
		c := exec.CommandContext(r.Context(), req.Cmd[0], req.Cmd[1:]...)
		c.Stdin = bytes.NewReader(req.Stdin)
		pr, pw, _ := os.Pipe()
		c.Stdout = pw
		var se bytes.Buffer
		c.Stderr = &se
		if err := c.Start(); err != nil {
			code := 127
			_ = enc.Encode(api.ExecEvent{Stream: "stderr", Data: []byte(err.Error())})
			_ = enc.Encode(api.ExecEvent{Exit: &code})
			return
		}
		pw.Close()
		// Trozos de tamaño caprichoso: el gateway debe rearmar las líneas.
		buf := make([]byte, 7)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				_ = enc.Encode(api.ExecEvent{Stream: "stdout", Data: append([]byte(nil), buf[:n]...)})
				fl.Flush()
			}
			if err != nil {
				break
			}
		}
		_ = c.Wait()
		if se.Len() > 0 {
			_ = enc.Encode(api.ExecEvent{Stream: "stderr", Data: se.Bytes()})
		}
		code := c.ProcessState.ExitCode()
		_ = enc.Encode(api.ExecEvent{Exit: &code})
	})
	m.HandleFunc("PUT /machines/{ref}/files", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		mode, _ := strconv.ParseUint(r.URL.Query().Get("mode"), 8, 32)
		if mode == 0 {
			mode = 0o644
		}
		b := new(bytes.Buffer)
		_, _ = b.ReadFrom(r.Body)
		if err := os.WriteFile(p, b.Bytes(), os.FileMode(mode)); err != nil {
			failDaemon(w, 500, err)
			return
		}
		writeJSON(w, 200, api.FileStat{Path: p, Size: int64(b.Len())})
	})
	m.HandleFunc("POST /machines/{ref}/processes", func(w http.ResponseWriter, r *http.Request) {
		var req api.ProcessStartRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		c := exec.Command(req.Cmd[0], req.Cmd[1:]...)
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := c.Start(); err != nil {
			failDaemon(w, 500, err)
			return
		}
		fin := make(chan struct{})
		go func() { _ = c.Wait(); close(fin) }()
		p := api.ProcessInfo{PID: c.Process.Pid, Cmd: req.Cmd, State: api.ProcessRunning, StartedAt: time.Now()}
		d.mu.Lock()
		d.procs[p.PID], d.info[p.PID] = fin, p
		d.mu.Unlock()
		t.Cleanup(func() { _ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL) })
		writeJSON(w, 201, p)
	})
	m.HandleFunc("GET /machines/{ref}/processes", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		out := []api.ProcessInfo{}
		for pid, p := range d.info {
			select {
			case <-d.procs[pid]:
				p.State = api.ProcessExited
			default:
			}
			out = append(out, p)
		}
		d.mu.Unlock()
		writeJSON(w, 200, out)
	})
	m.HandleFunc("DELETE /machines/{ref}/processes/{pid}", func(w http.ResponseWriter, r *http.Request) {
		pid, _ := strconv.Atoi(r.PathValue("pid"))
		d.mu.Lock()
		_, ok := d.procs[pid]
		delete(d.info, pid)
		d.mu.Unlock()
		if !ok {
			failDaemon(w, 404, os.ErrNotExist)
			return
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		w.WriteHeader(204)
	})
	m.Handle("/", proxy)
	srv := httptest.NewUnstartedServer(m)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	f.endpoint = sock
	return d
}

type bancoCodigo struct {
	srv  *httptest.Server
	d    *daemonReal
	base string
}

func montarCodigo(t *testing.T) *bancoCodigo {
	t.Helper()
	f := nuevoFalso(t)
	d := ponerReal(t, f)
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	return &bancoCodigo{srv: srv, d: d, base: "/v1/sandboxes/" + sb.ID}
}

func (b *bancoCodigo) contexto(t *testing.T) string {
	t.Helper()
	resp := pide(t, b.srv, tokenAlice, "POST", b.base+"/code/contexts", map[string]any{"language": "python"})
	esperaCodigo(t, resp, 201)
	c := decodifica[struct {
		ID       string `json:"id"`
		Language string `json:"language"`
	}](t, resp)
	if c.ID == "" || c.Language != "python" {
		t.Fatalf("context: %+v", c)
	}
	return c.ID
}

// corre manda código y devuelve los eventos del flujo.
func (b *bancoCodigo) corre(t *testing.T, ctxID, codigo string) []evCodigo {
	t.Helper()
	cuerpo := map[string]any{"code": codigo}
	if ctxID != "" {
		cuerpo["context_id"] = ctxID
	}
	resp := pide(t, b.srv, tokenAlice, "POST", b.base+"/code", cuerpo)
	esperaCodigo(t, resp, 200)
	defer resp.Body.Close()
	var evs []evCodigo
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var e evCodigo
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad NDJSON line %q: %v", sc.Text(), err)
		}
		evs = append(evs, e)
	}
	return evs
}

func texto(evs []evCodigo, tipo string) string {
	var s strings.Builder
	for _, e := range evs {
		if e.Type == tipo {
			s.WriteString(e.Text)
		}
	}
	return s.String()
}

// El estado persiste entre ejecuciones de un contexto, un error devuelve el
// traceback como evento y el flujo siempre acaba en done.
func TestCodigoEstadoYError(t *testing.T) {
	b := montarCodigo(t)
	id := b.contexto(t)

	evs := b.corre(t, id, "x = 1")
	if last := evs[len(evs)-1]; last.Type != "done" || last.OK == nil || !*last.OK {
		t.Fatalf("x = 1: %+v", evs)
	}
	evs = b.corre(t, id, "print(x)")
	if got := texto(evs, "stdout"); got != "1\n" {
		t.Fatalf("print(x) stdout %q, want 1\\n (%+v)", got, evs)
	}
	evs = b.corre(t, id, "x + 41")
	if got := texto(evs, "result"); got != "42" {
		t.Fatalf("last expression: result %q (%+v)", got, evs)
	}

	evs = b.corre(t, id, "print('antes')\n1/0")
	var errEv *evCodigo
	for i := range evs {
		if evs[i].Type == "error" {
			errEv = &evs[i]
		}
	}
	if errEv == nil || errEv.Ename != "ZeroDivisionError" || !strings.Contains(strings.Join(errEv.Traceback, "\n"), "division by zero") {
		t.Fatalf("error event: %+v", evs)
	}
	if strings.Contains(strings.Join(errEv.Traceback, "\n"), "conductor") {
		t.Errorf("the traceback leaks the driver's frames: %v", errEv.Traceback)
	}
	if texto(evs, "stdout") != "antes\n" || evs[len(evs)-1].Type != "done" || *evs[len(evs)-1].OK {
		t.Fatalf("output before the error or done event missing: %+v", evs)
	}
	// Tras un error el contexto sigue vivo y con su estado.
	if got := texto(b.corre(t, id, "print(x)"), "stdout"); got != "1\n" {
		t.Fatalf("after an error: %q", got)
	}
	// Sintaxis rota: error, no 500.
	evs = b.corre(t, id, "def (")
	if evs[0].Type != "error" || evs[0].Ename != "SyntaxError" {
		t.Fatalf("syntax error: %+v", evs)
	}
}

// Sin contexto, la ejecución es efímera: no deja estado ni procesos.
func TestCodigoEfimero(t *testing.T) {
	b := montarCodigo(t)
	if got := texto(b.corre(t, "", "print(6*7)"), "stdout"); got != "42\n" {
		t.Fatalf("ephemeral: %q", got)
	}
	if got := b.corre(t, "", "print('x' in dir())"); texto(got, "stdout") != "False\n" {
		t.Fatalf("an ephemeral run kept state: %+v", got)
	}
	resp := pide(t, b.srv, tokenAlice, "GET", b.base+"/code/contexts", nil)
	esperaCodigo(t, resp, 200)
	if l := decodifica[[]map[string]any](t, resp); len(l) != 0 {
		t.Fatalf("ephemeral runs left contexts: %v", l)
	}
}

// Un plazo vencido corta la ejecución con TimeoutError y el contexto sigue.
func TestCodigoPlazo(t *testing.T) {
	b := montarCodigo(t)
	id := b.contexto(t)
	resp := pide(t, b.srv, tokenAlice, "POST", b.base+"/code",
		map[string]any{"code": "import time\ny = 5\ntime.sleep(30)", "context_id": id, "timeout_seconds": 1})
	esperaCodigo(t, resp, 200)
	defer resp.Body.Close()
	var visto bool
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var e evCodigo
		_ = json.Unmarshal(sc.Bytes(), &e)
		visto = visto || (e.Type == "error" && e.Ename == "TimeoutError")
	}
	if !visto {
		t.Fatal("no TimeoutError event")
	}
	if got := texto(b.corre(t, id, "print(y)"), "stdout"); got != "5\n" {
		t.Fatalf("state after a timeout: %q", got)
	}
}

// Un contexto de otro inquilino da 404 en todas las rutas; borrar el propio
// mata su proceso.
func TestCodigoAjenoYBorrado(t *testing.T) {
	b := montarCodigo(t)
	id := b.contexto(t)

	for _, r := range []struct{ m, p string }{
		{"POST", "/code/contexts"}, {"GET", "/code/contexts"}, {"GET", "/code/contexts/" + id},
		{"DELETE", "/code/contexts/" + id}, {"POST", "/code"},
	} {
		var cuerpo any
		if r.p == "/code" {
			cuerpo = map[string]any{"code": "1", "context_id": id}
		}
		resp := pide(t, b.srv, tokenBob, r.m, b.base+r.p, cuerpo)
		esperaCodigo(t, resp, 404)
	}
	// Y un id que no existe en el sandbox propio también.
	resp := pide(t, b.srv, tokenAlice, "POST", b.base+"/code", map[string]any{"code": "1", "context_id": "000000000000"})
	esperaCodigo(t, resp, 404)

	var pid int
	b.d.mu.Lock()
	for p, inf := range b.d.info {
		if len(inf.Cmd) > 1 && strings.HasSuffix(inf.Cmd[1], "conductor.py") {
			pid = p
		}
	}
	b.d.mu.Unlock()
	if pid == 0 || !b.d.vivo(pid) {
		t.Fatalf("the context's process isn't running (pid %d)", pid)
	}
	esperaCodigo(t, pide(t, b.srv, tokenAlice, "DELETE", b.base+"/code/contexts/"+id, nil), 204)
	for i := 0; i < 100 && b.d.vivo(pid); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if b.d.vivo(pid) {
		t.Fatal("deleting the context didn't kill its process")
	}
	esperaCodigo(t, pide(t, b.srv, tokenAlice, "GET", b.base+"/code/contexts/"+id, nil), 404)
}

// Tope de contextos por sandbox, de tamaño de código y de lenguaje.
func TestCodigoTopes(t *testing.T) {
	b := montarCodigo(t)
	for i := 0; i < 8; i++ {
		b.contexto(t)
	}
	esperaCodigo(t, pide(t, b.srv, tokenAlice, "POST", b.base+"/code/contexts", map[string]any{"language": "python"}), 429)

	grande := strings.Repeat("a", api.ExecMaxStdin+1)
	esperaCodigo(t, pide(t, b.srv, tokenAlice, "POST", b.base+"/code", map[string]any{"code": grande}), 413)
	esperaCodigo(t, pide(t, b.srv, tokenAlice, "POST", b.base+"/code", map[string]any{"code": "1", "language": "cobol"}), 400)
	esperaCodigo(t, pide(t, b.srv, tokenAlice, "POST", b.base+"/code/contexts", map[string]any{"language": "cobol"}), 400)
	esperaCodigo(t, pide(t, b.srv, tokenAlice, "POST", b.base+"/code", map[string]any{"code": ""}), 400)
	esperaCodigo(t, pide(t, b.srv, tokenAlice, "GET", b.base+"/code", nil), 405)
}

// Sin python3 en la imagen: 422 con un mensaje claro, tanto al crear un
// contexto como en una ejecución efímera.
func TestCodigoSinPython(t *testing.T) {
	b := montarCodigo(t)
	b.d.mu.Lock()
	b.d.sinPython = true
	b.d.mu.Unlock()
	for _, c := range []struct {
		p string
		b any
	}{
		{"/code/contexts", map[string]any{"language": "python"}},
		{"/code", map[string]any{"code": "1"}},
	} {
		resp := pide(t, b.srv, tokenAlice, "POST", b.base+c.p, c.b)
		esperaCodigo(t, resp, 422)
		if e := decodifica[api.Error](t, resp); !strings.Contains(e.Message, "python3") {
			t.Errorf("%s: message %q doesn't say python3", c.p, e.Message)
		}
	}
}

// El conductor, aparte del gateway: hola, estado, plazo y cliente.
func TestConductorAparte(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 in this runner")
	}
	dir, err := os.MkdirTemp("", "cd")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	script := filepath.Join(dir, "conductor.py")
	src, err := os.ReadFile("codigo_conductor.py")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, src, 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "s.sock")
	srv := exec.Command(py, "-I", script, "serve", sock)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Process.Kill(); _ = srv.Wait() }()

	run := func(codigo, plazo string) []evCodigo {
		c := exec.Command(py, "-I", script, "run", sock, plazo)
		c.Stdin = strings.NewReader(codigo)
		out, _ := c.Output()
		var evs []evCodigo
		for _, l := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
			var e evCodigo
			if err := json.Unmarshal(l, &e); err != nil {
				t.Fatalf("line %q: %v", l, err)
			}
			evs = append(evs, e)
		}
		return evs
	}
	run("a = [1, 2]", "0")
	if got := texto(run("print(sum(a))\nimport sys\nprint('e', file=sys.stderr)", "0"), "stdout"); got != "3\n" {
		t.Fatalf("stdout %q", got)
	}
	// Un `except Exception` del usuario no se traga el plazo.
	evs := run("while True:\n    try:\n        pass\n    except Exception:\n        pass", "1")
	var tout bool
	for _, e := range evs {
		tout = tout || e.Ename == "TimeoutError"
	}
	if !tout {
		t.Fatalf("no timeout: %+v", evs)
	}
	// El permiso del socket es del dueño.
	if st, err := os.Stat(sock); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("socket mode: %v %v", st, err)
	}
}
