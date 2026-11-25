package frontal

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
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
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/e2b"
	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/ext/sandbox/internal/plantilla"
	"github.com/juan52878911/kindling/pkg/api"
)

// VersionE2B es la versión del SDK de E2B para Python que fija docs/e2b.md (y
// scripts/e2b-sdk-real/e2b_real.py); subirla es un cambio de compatibilidad.
const VersionE2B = "2.64.0"

// VersionE2BCodeInterpreter es la de e2b-code-interpreter (scripts/e2b-sdk-real/e2b_ci.py).
const VersionE2BCodeInterpreter = "2.11.10"

// procE2B es un proceso de fondo del daemon falso de ponerE2B: guarda su
// salida por trozos (stdout y stderr) para servir output con offset y follow.
type procE2B struct {
	info   api.ProcessInfo
	trozos []api.ProcessEvent // Offset es el desplazamiento siguiente al trozo
	total  int64
	fin    chan struct{}
}

// salidaE2B escribe en un proceso de procE2B bajo el cerrojo del daemon.
type salidaE2B struct {
	mu     *sync.Mutex
	cambio *sync.Cond
	p      *procE2B
	stream string
}

func (s salidaE2B) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.p.total += int64(len(b))
	s.p.trozos = append(s.p.trozos, api.ProcessEvent{Stream: s.stream, Data: append([]byte(nil), b...), Offset: s.p.total})
	s.p.info.OutputBytes = s.p.total
	s.cambio.Broadcast()
	return len(b), nil
}

// ponerE2B pone delante del daemon falso (con ponerReal y ponerOSB) los
// procesos de fondo completos que usa el envd del shim de E2B: dir y env al
// arrancar, salida con offset y follow que acaba en exit, señales al grupo, y
// listar y borrar.
func ponerE2B(t *testing.T, f *falso) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pe")
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
	var mu sync.Mutex
	cambio := sync.NewCond(&mu)
	procs := map[int]*procE2B{}
	buscar := func(r *http.Request) *procE2B {
		pid, _ := strconv.Atoi(r.PathValue("pid"))
		mu.Lock()
		defer mu.Unlock()
		return procs[pid]
	}
	// El env de create es la sesión "default" de la máquina, que aplican los
	// procesos sin sesión (como hace el agente del invitado).
	sesiones := map[string][]string{}
	m := http.NewServeMux()
	m.HandleFunc("POST /machines/{ref}/sessions", func(w http.ResponseWriter, r *http.Request) {
		var req api.SessionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		sesiones[r.PathValue("ref")+"/"+req.ID] = req.Env
		mu.Unlock()
		writeJSON(w, 201, &api.Session{ID: req.ID, Dir: req.Dir, Env: req.Env})
	})
	m.HandleFunc("POST /machines/{ref}/processes", func(w http.ResponseWriter, r *http.Request) {
		var req api.ProcessStartRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Cmd) == 0 {
			failDaemon(w, 400, errors.New("cmd is required"))
			return
		}
		if req.Session == "" {
			req.Session = "default"
		}
		mu.Lock()
		env := append(append(os.Environ(), sesiones[r.PathValue("ref")+"/"+req.Session]...), req.Env...)
		mu.Unlock()
		p := &procE2B{fin: make(chan struct{})}
		c := exec.Command(req.Cmd[0], req.Cmd[1:]...)
		c.Dir, c.Env = req.Dir, env
		c.Stdout = salidaE2B{&mu, cambio, p, "stdout"}
		c.Stderr = salidaE2B{&mu, cambio, p, "stderr"}
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := c.Start(); err != nil {
			failDaemon(w, 500, err)
			return
		}
		mu.Lock()
		p.info = api.ProcessInfo{PID: c.Process.Pid, Cmd: req.Cmd, Dir: req.Dir, State: api.ProcessRunning, StartedAt: time.Now()}
		procs[p.info.PID] = p
		info := p.info
		mu.Unlock()
		go func() {
			_ = c.Wait()
			mu.Lock()
			ws := c.ProcessState.Sys().(syscall.WaitStatus)
			code := ws.ExitStatus()
			if ws.Signaled() {
				code = 128 + int(ws.Signal())
				if p.info.Signal == "" {
					p.info.Signal = strings.TrimPrefix(strings.ToUpper(ws.Signal().String()), "SIG")
				}
			}
			ahora := time.Now()
			p.info.State, p.info.ExitCode, p.info.EndedAt = api.ProcessExited, &code, &ahora
			cambio.Broadcast()
			mu.Unlock()
			close(p.fin)
		}()
		t.Cleanup(func() { _ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL); <-p.fin })
		writeJSON(w, 201, info)
	})
	m.HandleFunc("GET /machines/{ref}/processes", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		out := []api.ProcessInfo{}
		for _, p := range procs {
			out = append(out, p.info)
		}
		mu.Unlock()
		writeJSON(w, 200, out)
	})
	m.HandleFunc("GET /machines/{ref}/processes/{pid}", func(w http.ResponseWriter, r *http.Request) {
		p := buscar(r)
		if p == nil {
			failDaemon(w, 404, os.ErrNotExist)
			return
		}
		mu.Lock()
		info := p.info
		mu.Unlock()
		writeJSON(w, 200, info)
	})
	m.HandleFunc("DELETE /machines/{ref}/processes/{pid}", func(w http.ResponseWriter, r *http.Request) {
		p := buscar(r)
		if p == nil {
			failDaemon(w, 404, os.ErrNotExist)
			return
		}
		_ = syscall.Kill(-p.info.PID, syscall.SIGKILL)
		mu.Lock()
		delete(procs, p.info.PID)
		mu.Unlock()
		w.WriteHeader(204)
	})
	m.HandleFunc("POST /machines/{ref}/processes/{pid}/signal", func(w http.ResponseWriter, r *http.Request) {
		p := buscar(r)
		if p == nil {
			failDaemon(w, 404, os.ErrNotExist)
			return
		}
		var req api.ProcessSignalRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		nombre := strings.TrimPrefix(strings.ToUpper(req.Signal), "SIG")
		sig := syscall.SIGTERM
		switch nombre {
		case "KILL":
			sig = syscall.SIGKILL
		case "", "TERM":
			nombre = "TERM"
		default:
			failDaemon(w, 400, errors.New("unsupported signal "+req.Signal))
			return
		}
		mu.Lock()
		if p.info.State == api.ProcessRunning {
			p.info.Signal = nombre
		}
		info := p.info
		mu.Unlock()
		_ = syscall.Kill(-p.info.PID, sig)
		writeJSON(w, 200, info)
	})
	m.HandleFunc("GET /machines/{ref}/processes/{pid}/output", func(w http.ResponseWriter, r *http.Request) {
		p := buscar(r)
		if p == nil {
			failDaemon(w, 404, os.ErrNotExist)
			return
		}
		q := r.URL.Query()
		offset, _ := strconv.ParseInt(q.Get("offset"), 10, 64)
		follow := q.Get("follow") == "1"
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		enc := json.NewEncoder(w)
		fl := w.(http.Flusher)
		// Despierta a la espera si el cliente se va.
		parar := context.AfterFunc(r.Context(), func() { mu.Lock(); cambio.Broadcast(); mu.Unlock() })
		defer parar()
		visto := 0
		for {
			mu.Lock()
			for follow && visto == len(p.trozos) && p.info.State == api.ProcessRunning && r.Context().Err() == nil {
				cambio.Wait()
			}
			nuevos := append([]api.ProcessEvent(nil), p.trozos[visto:]...)
			visto = len(p.trozos)
			info := p.info
			mu.Unlock()
			for _, ev := range nuevos {
				if ev.Offset <= offset {
					continue
				}
				if inicio := ev.Offset - int64(len(ev.Data)); inicio < offset {
					ev.Data = ev.Data[offset-inicio:]
				}
				_ = enc.Encode(ev)
			}
			fl.Flush()
			if r.Context().Err() != nil {
				return
			}
			if !follow {
				return
			}
			if info.State != api.ProcessRunning {
				_ = enc.Encode(api.ProcessEvent{Offset: info.OutputBytes, Exit: info.ExitCode, Signal: info.Signal})
				return
			}
		}
	})
	m.Handle("/", proxy)
	srv := httptest.NewUnstartedServer(m)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	f.endpoint = sock
}

// saltarE2B salta el subtest si falta el SDK, salvo con KLING_E2B_REQUIRED=1
// (CI), donde es un fallo.
func saltarE2B(t *testing.T, f string, a ...any) {
	t.Helper()
	if os.Getenv("KLING_E2B_REQUIRED") == "1" {
		t.Fatalf(f, a...)
	}
	t.Skipf(f, a...)
}

// sdkE2B es cómo correr el script de un SDK: el intérprete y sus argumentos
// antes de los del script, o vacío si ese SDK no está en esta máquina.
//
//   - python: $KLING_E2B_PYTHON (un venv con `pip install e2b==VersionE2B`) o python3.
//   - js: node, con el paquete en $KLING_E2B_NODE (un directorio con
//     `npm i e2b@VersionE2B`).
func sdkE2B(t *testing.T, sdk string) []string {
	t.Helper()
	switch sdk {
	case "python", "code-interpreter":
		py := os.Getenv("KLING_E2B_PYTHON")
		if py == "" {
			py = "python3"
		}
		bin, err := exec.LookPath(py)
		if err != nil {
			saltarE2B(t, "no %s in this runner", py)
		}
		if sdk == "code-interpreter" {
			if err := exec.Command(bin, "-c", "import e2b_code_interpreter").Run(); err != nil {
				saltarE2B(t, "%s has no e2b-code-interpreter package (pip install e2b-code-interpreter==%s, or set KLING_E2B_PYTHON)", py, VersionE2BCodeInterpreter)
			}
			return []string{bin, "e2b_ci.py"}
		}
		if err := exec.Command(bin, "-c", "import e2b").Run(); err != nil {
			saltarE2B(t, "%s has no e2b package (pip install e2b==%s, or set KLING_E2B_PYTHON)", py, VersionE2B)
		}
		return []string{bin, "e2b_real.py"}
	default:
		dir := os.Getenv("KLING_E2B_NODE")
		if dir == "" {
			saltarE2B(t, "KLING_E2B_NODE is not set (a directory with `npm i e2b@%s`)", VersionE2B)
		}
		bin, err := exec.LookPath("node")
		if err != nil {
			saltarE2B(t, "no node in this runner")
		}
		if _, err := os.Stat(filepath.Join(dir, "node_modules", "e2b", "package.json")); err != nil {
			saltarE2B(t, "%s has no node_modules/e2b (npm i e2b@%s)", dir, VersionE2B)
		}
		return []string{bin, "e2b_real.mjs"}
	}
}

// claveE2B es la API key de E2B que aceptan los shims de TestContratoE2B.
const claveE2B = "clave-e2b-de-prueba-0123"

// dominioE2B es el E2B_DOMAIN de los subtest con dominio.
const dominioE2B = "e2b.test"

// certE2B hace una CA y un certificado de *.dominioE2B firmado por ella, y deja
// la CA en un PEM para el SSL_CERT_FILE del SDK.
func certE2B(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	claveCA, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	plantillaCA := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kindling e2b test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	derCA, err := x509.CreateCertificate(rand.Reader, plantillaCA, plantillaCA, &claveCA.PublicKey, claveCA)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(derCA)
	claveHoja, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	hoja := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "*." + dominioE2B},
		DNSNames: []string{"*." + dominioE2B}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	derHoja, err := x509.CreateCertificate(rand.Reader, hoja, ca, &claveHoja.PublicKey, claveCA)
	if err != nil {
		t.Fatal(err)
	}
	pemCA := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(pemCA, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derCA}), 0o644); err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{derHoja}, PrivateKey: claveHoja}, pemCA
}

// proxyE2B es un proxy HTTP CONNECT que manda todo *.dominioE2B:443 a destino:
// hace de DNS comodín para el SDK, que sigue hablando TLS con el nombre real.
func proxyE2B(t *testing.T, destino string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, puerto, _ := net.SplitHostPort(r.Host)
		if r.Method != http.MethodConnect || puerto != "443" || !strings.HasSuffix(host, "."+dominioE2B) {
			http.Error(w, "only CONNECT to *."+dominioE2B+":443", http.StatusForbidden)
			return
		}
		dst, err := net.Dial("tcp", destino)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			dst.Close()
			return
		}
		_, _ = c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		go func() { _, _ = io.Copy(dst, c); dst.Close() }()
		_, _ = io.Copy(c, dst)
		c.Close()
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// bancoE2B monta el frontal real delante del daemon falso y devuelve su URL y
// un shim de E2B sobre él (sin listener: cada subtest lo sirve donde quiere).
// logs da lo que el shim registró.
func bancoE2B(t *testing.T) (gw string, shim *e2b.Servidor, logs func() string) {
	t.Helper()
	f := nuevoFalso(t)
	ponerReal(t, f)
	ponerOSB(t, f)
	ponerE2B(t, f)
	f.mu.Lock()
	f.snapshots = append(f.snapshots, plantilla.SnapshotDe("base"))
	f.mu.Unlock()
	s, err := Nuevo(Opciones{
		Hosts:   hosts.Nuevo(map[string]string{"a": f.endpoint}),
		Tenants: []Tenant{{Nombre: "alice", Token: tokenAlice}},
		Log:     testLog(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	// El shim registra desde cada petición en paralelo.
	var (
		mu sync.Mutex
		b  strings.Builder
	)
	shim = &e2b.Servidor{API: apiFrontal{srv.URL, tokenAlice}, Clave: claveE2B,
		Log: func(l string) {
			mu.Lock()
			defer mu.Unlock()
			b.WriteString(l + "\n")
		}}
	return srv.URL, shim, func() string { mu.Lock(); defer mu.Unlock(); return b.String() }
}

// correrE2B corre un script de scripts/e2b-sdk-real y exige "E2B OK". env se
// añade al entorno del test.
func correrE2B(t *testing.T, logs func() string, env []string, cmd ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	c.Env = append(os.Environ(), env...)
	out, err := c.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "E2B OK") {
		t.Fatalf("SDK de E2B: %v\n%s\nshim:\n%s", err, out, logs())
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}

// TestContratoE2B corre los SDK reales de E2B (Python y JS:
// scripts/e2b-sdk-real/) contra `kling sbx e2b` montado delante del frontal
// REAL, con su daemon falso que ejecuta las órdenes, los procesos de fondo y
// los ficheros en esta máquina: crear, commands.run (salida, exit, env y cwd,
// fondo, kill, connect), ficheros, listar, info, timeout, red, pausa, connect y
// kill; y el intérprete de e2b-code-interpreter. Los subtest *-domain repiten
// los de Python con solo E2B_DOMAIN (HTTPS a api.<dominio> y a
// <puerto>-<id>.<dominio>, con un certificado propio y un proxy CONNECT en lugar
// del DNS comodín); python-debug, los pasos de envd con E2B_DEBUG=true (envd en
// localhost:49983, sin plano de control). Sin VM.
func TestContratoE2B(t *testing.T) {
	_, este, _, _ := runtime.Caller(0)
	scripts := filepath.Join(filepath.Dir(este), "..", "..", "..", "..", "scripts", "e2b-sdk-real")
	for _, sdk := range []string{"python", "js", "code-interpreter"} {
		t.Run(sdk, func(t *testing.T) {
			arg := sdkE2B(t, sdk)
			_, shim, logs := bancoE2B(t)
			srv := httptest.NewServer(shim)
			t.Cleanup(srv.Close)
			correrE2B(t, logs, nil, arg[0], filepath.Join(scripts, arg[1]), srv.URL, claveE2B, t.TempDir(), "base")
		})
	}
	for _, sdk := range []string{"python", "code-interpreter"} {
		t.Run(sdk+"-domain", func(t *testing.T) {
			arg := sdkE2B(t, sdk)
			_, shim, logs := bancoE2B(t)
			shim.Dominio = dominioE2B
			cert, ca := certE2B(t)
			srv := httptest.NewUnstartedServer(shim)
			srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
			srv.StartTLS()
			t.Cleanup(srv.Close)
			proxy := proxyE2B(t, srv.Listener.Addr().String())
			// SSL_CERT_FILE: la CA del certificado; los proxies del entorno, fuera.
			env := []string{"SSL_CERT_FILE=" + ca, "E2B_REAL_PROXY=" + proxy, "HTTPS_PROXY=", "HTTP_PROXY=",
				"https_proxy=", "http_proxy=", "ALL_PROXY=", "all_proxy=", "NO_PROXY=", "no_proxy="}
			correrE2B(t, logs, env, arg[0], filepath.Join(scripts, arg[1]), "--domain", dominioE2B, claveE2B, t.TempDir(), "base")
		})
	}
	t.Run("python-debug", func(t *testing.T) {
		arg := sdkE2B(t, "python")
		// El SDK en modo debug va siempre a localhost:49983.
		ln, err := net.Listen("tcp", "127.0.0.1:49983")
		if err != nil {
			t.Skipf("127.0.0.1:49983 is busy: %v", err)
		}
		gw, shim, logs := bancoE2B(t)
		resp, err := apiFrontal{gw, tokenAlice}.Do(context.Background(), "POST", "/v1/sandboxes",
			strings.NewReader(`{"template":"base"}`), "application/json")
		if err != nil {
			t.Fatal(err)
		}
		var sb struct{ ID string }
		_ = json.NewDecoder(resp.Body).Decode(&sb)
		resp.Body.Close()
		if resp.StatusCode != 201 || sb.ID == "" {
			t.Fatalf("crear el sandbox de debug: %d %+v", resp.StatusCode, sb)
		}
		shim.SandboxDebug = sb.ID
		srv := httptest.NewUnstartedServer(shim.HandlerDebug())
		srv.Listener.Close()
		srv.Listener = ln
		srv.Start()
		t.Cleanup(srv.Close)
		correrE2B(t, logs, nil, arg[0], filepath.Join(scripts, arg[1]), "--debug", t.TempDir())
	})
}
