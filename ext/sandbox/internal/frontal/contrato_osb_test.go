package frontal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/ext/sandbox/internal/osb"
	"github.com/juan52878911/kindling/ext/sandbox/internal/plantilla"
	"github.com/juan52878911/kindling/pkg/api"
)

// apiFrontal es el cliente que el shim de OSB usa contra este frontal: el mismo
// contrato que el cliente de `kling sbx`.
type apiFrontal struct{ base, token string }

func (a apiFrontal) Do(ctx context.Context, metodo, ruta string, cuerpo io.Reader, tipo string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, metodo, a.base+ruta, cuerpo)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if cuerpo != nil {
		req.Header.Set("Content-Type", tipo)
	}
	return http.DefaultClient.Do(req)
}

// ponerOSB pone delante del daemon falso (con ponerReal) lo que el SDK de OSB
// comprueba y los falsos de siempre no guardan: pausa, thaw y egress que cambian
// la máquina guardada (get_info y get_egress_policy los releen), el modo real de
// un fichero en stat, y procesos de fondo que guardan su salida (status y logs).
func ponerOSB(t *testing.T, f *falso) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no unix sockets here: %v", err)
	}
	detras := f.endpoint
	proxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) { r.URL.Scheme, r.URL.Host = "http", "daemon" },
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", detras)
		}},
	}
	estado := func(st api.State) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			mc, ok := f.maquinas[r.PathValue("ref")]
			var c api.Machine
			if ok {
				mc.State = st
				c = *mc
			}
			f.mu.Unlock()
			if !ok {
				failDaemon(w, 404, os.ErrNotExist)
				return
			}
			writeJSON(w, 200, &c)
		}
	}
	m := http.NewServeMux()
	m.HandleFunc("POST /machines/{ref}/pause", estado(api.StatePaused))
	m.HandleFunc("POST /machines/{ref}/thaw", estado(api.StateRunning))
	m.HandleFunc("PATCH /machines/{ref}/egress", func(w http.ResponseWriter, r *http.Request) {
		var req api.EgressRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		if mc, ok := f.maquinas[r.PathValue("ref")]; ok {
			mc.Egress, mc.AllowDomains = req.Egress, req.AllowDomains
		}
		f.mu.Unlock()
		writeJSON(w, 200, api.EgressPolicy{Egress: req.Egress, AllowDomains: req.AllowDomains, Applied: api.EgressAppliedLive})
	})
	m.HandleFunc("GET /machines/{ref}/files", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if fi, err := os.Stat(q.Get("path")); err == nil && q.Get("stat") == "1" {
			writeJSON(w, 200, api.FileStat{Path: q.Get("path"), Size: fi.Size(), Mode: fmt.Sprintf("%04o", fi.Mode().Perm()),
				IsDir: fi.IsDir(), ModTime: fi.ModTime()})
			return
		}
		if q.Get("list") == "1" {
			ents, err := os.ReadDir(q.Get("path"))
			if err != nil {
				failDaemon(w, 404, err)
				return
			}
			l := api.FileList{Path: q.Get("path"), Entries: []api.FileEntry{}}
			for _, e := range ents {
				fi, err := e.Info()
				if err != nil {
					continue
				}
				tipo := "file"
				if fi.IsDir() {
					tipo = "dir"
				}
				l.Entries = append(l.Entries, api.FileEntry{Name: e.Name(), Type: tipo, Size: fi.Size(),
					Mode: fmt.Sprintf("%04o", fi.Mode().Perm()), ModTime: fi.ModTime()})
			}
			writeJSON(w, 200, l)
			return
		}
		proxy.ServeHTTP(w, r)
	})
	type proc struct {
		info api.ProcessInfo
		out  bytes.Buffer
		fin  chan struct{}
	}
	var mu sync.Mutex
	procs := map[int]*proc{}
	m.HandleFunc("POST /machines/{ref}/processes", func(w http.ResponseWriter, r *http.Request) {
		var req api.ProcessStartRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		p := &proc{fin: make(chan struct{})}
		c := exec.Command(req.Cmd[0], req.Cmd[1:]...)
		c.Stdout, c.Stderr = &escritorSeguro{mu: &mu, b: &p.out}, &escritorSeguro{mu: &mu, b: &p.out}
		if err := c.Start(); err != nil {
			failDaemon(w, 500, err)
			return
		}
		p.info = api.ProcessInfo{PID: c.Process.Pid, Cmd: req.Cmd, State: api.ProcessRunning, StartedAt: time.Now()}
		mu.Lock()
		procs[p.info.PID] = p
		mu.Unlock()
		go func() {
			err := c.Wait()
			mu.Lock()
			code := 0
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			}
			ahora := time.Now()
			p.info.State, p.info.ExitCode, p.info.EndedAt = api.ProcessExited, &code, &ahora
			mu.Unlock()
			close(p.fin)
		}()
		t.Cleanup(func() { _ = c.Process.Kill(); <-p.fin })
		writeJSON(w, 201, p.info)
	})
	m.HandleFunc("GET /machines/{ref}/processes/{pid}", func(w http.ResponseWriter, r *http.Request) {
		pid, _ := strconv.Atoi(r.PathValue("pid"))
		mu.Lock()
		p := procs[pid]
		var info api.ProcessInfo
		if p != nil {
			info = p.info
		}
		mu.Unlock()
		if p == nil {
			failDaemon(w, 404, os.ErrNotExist)
			return
		}
		writeJSON(w, 200, info)
	})
	m.HandleFunc("GET /machines/{ref}/processes/{pid}/output", func(w http.ResponseWriter, r *http.Request) {
		pid, _ := strconv.Atoi(r.PathValue("pid"))
		mu.Lock()
		p := procs[pid]
		var datos []byte
		if p != nil {
			datos = append(datos, p.out.Bytes()...)
		}
		mu.Unlock()
		if p == nil {
			failDaemon(w, 404, os.ErrNotExist)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(api.ProcessEvent{Stream: "stdout", Data: datos, Offset: int64(len(datos))})
	})
	m.Handle("/", proxy)
	srv := httptest.NewUnstartedServer(m)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	f.endpoint = sock
}

// escritorSeguro escribe en un buffer bajo un cerrojo compartido.
type escritorSeguro struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (e *escritorSeguro) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.b.Write(p)
}

// pythonOSB es el intérprete con el paquete `opensandbox`: $KLING_OSB_PYTHON
// (un venv) o python3. Con KLING_OSB_REQUIRED=1 (CI) que falte es un fallo.
func pythonOSB(t *testing.T) string {
	t.Helper()
	saltar := func(f string, a ...any) {
		t.Helper()
		if os.Getenv("KLING_OSB_REQUIRED") == "1" {
			t.Fatalf(f, a...)
		}
		t.Skipf(f, a...)
	}
	py := os.Getenv("KLING_OSB_PYTHON")
	if py == "" {
		py = "python3"
	}
	bin, err := exec.LookPath(py)
	if err != nil {
		saltar("no %s in this runner", py)
	}
	if err := exec.Command(bin, "-c", "import opensandbox").Run(); err != nil {
		saltar("%s has no opensandbox package (pip install opensandbox==%s, or set KLING_OSB_PYTHON)", py, osb.VersionOSB)
	}
	return bin
}

// TestContratoOSB corre el SDK real de OpenSandbox (scripts/osb-sdk-real/
// osb_real.py) contra `kling sbx osb` montado delante del frontal REAL, con su
// daemon falso que ejecuta las órdenes y los ficheros en esta máquina: crear,
// commands.run, escribir y leer, pausar, reanudar y borrar. Sin VM.
func TestContratoOSB(t *testing.T) {
	py := pythonOSB(t)
	_, este, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(este), "..", "..", "..", "..", "scripts", "osb-sdk-real", "osb_real.py")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("falta el script del SDK real: %v", err)
	}
	f := nuevoFalso(t)
	ponerSesiones(t, f)
	ponerReal(t, f)
	ponerOSB(t, f)
	f.mu.Lock()
	f.snapshots = append(f.snapshots, plantilla.SnapshotDe("py"))
	f.mu.Unlock()
	s, err := Nuevo(Opciones{
		Hosts:   hosts.Nuevo(map[string]string{"a": f.endpoint}),
		Tenants: []Tenant{{Nombre: "alice", Token: tokenAlice}},
		Log:     testLog(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(s)
	t.Cleanup(gw.Close)
	const clave = "clave-osb-de-prueba-0123"
	var logs strings.Builder
	shim := httptest.NewServer(&osb.Servidor{API: apiFrontal{gw.URL, tokenAlice}, Clave: clave,
		Imagenes: map[string]string{"python:3.12": "py"},
		Log:      func(l string) { logs.WriteString(l + "\n") }})
	t.Cleanup(shim.Close)

	base := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, py, script, shim.URL, clave, base, "python:3.12")
	cmd.Env = append(os.Environ(), "OPEN_SANDBOX_API_KEY=", "OPEN_SANDBOX_DOMAIN=")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "OSB OK") {
		t.Fatalf("SDK de OSB: %v\n%s\nshim:\n%s", err, out, logs.String())
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}
