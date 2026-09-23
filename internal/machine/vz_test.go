//go:build darwin

package machine

// Pruebas del camino de macOS contra un kling-vz FALSO: el propio binario de
// pruebas, relanzado como VMM, sirve por el socket unix las rutas que el núcleo
// usa y apunta en un fichero cada petición que recibe. Así se ejercitan spawn,
// boot y Thaw de verdad —proceso aparte, socket, orden de las llamadas— sin
// Virtualization.framework ni una VM.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

const envFakeVZ = "KLING_FAKE_VZ_LOG"

func TestMain(m *testing.M) {
	// Relanzado como VMM: no hay pruebas que correr, solo servir. Va antes de
	// m.Run() porque este proceso recibe "--api-sock", que el paquete testing
	// no entiende.
	if log := os.Getenv(envFakeVZ); log != "" {
		for i, a := range os.Args {
			if a == "--api-sock" && i+1 < len(os.Args) {
				servirVZFalso(os.Args[i+1], log)
				os.Exit(0)
			}
		}
	}
	os.Exit(m.Run())
}

// puertoFalso es el puerto de loopback que el falso "abre" para cada puerto
// del invitado. Determinista, para poder comprobarlo desde la prueba.
func puertoFalso(p int) string { return "127.0.0.1:" + strconv.Itoa(40000+p%10000) }

func servirVZFalso(sock, logPath string) {
	ln, err := net.Listen("unix", sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake kling-vz:", err)
		os.Exit(1)
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		os.Exit(1)
	}
	var mu sync.Mutex
	apuntar := func(linea string) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintln(f, linea)
		_ = f.Sync()
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		linea := r.Method + " " + r.URL.Path
		switch r.URL.Path {
		case "/kling/network":
			var n struct {
				Egress string `json:"egress"`
			}
			_ = json.Unmarshal(body, &n)
			linea += " egress=" + n.Egress
		case "/kling/forwards":
			var q struct {
				Ports []int `json:"ports"`
			}
			_ = json.Unmarshal(body, &q)
			fwd := map[string]string{}
			for _, p := range q.Ports {
				fwd[strconv.Itoa(p)] = puertoFalso(p)
			}
			apuntar(linea)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"forwards": fwd})
			return
		case "/kling/stats":
			apuntar(linea)
			_, _ = io.WriteString(w, `{"footprint_mib": 321}`)
			return
		}
		apuntar(linea)
		w.WriteHeader(http.StatusNoContent)
	})
	_ = http.Serve(ln, h)
}

// managerVZ monta un Manager mínimo cuyo VMM es el falso.
func managerVZ(t *testing.T) (*Manager, string) {
	t.Helper()
	// El socket unix no admite rutas largas, y el TempDir de las pruebas en
	// macOS se pasa: se usa uno corto propio.
	root, err := os.MkdirTemp("/tmp", "kvz")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	logPath := filepath.Join(root, "calls.log")
	t.Setenv(envFakeVZ, logPath)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		root: root, fcBin: exe, priv: &Privileges{}, bus: events.New(),
		byID:   map[string]*api.Machine{},
		socket: map[string]string{},
	}
	return m, logPath
}

func llamadas(t *testing.T, logPath string) []string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// indice devuelve la posición de la primera llamada que empieza por pre.
func indice(ls []string, pre string) int {
	for i, l := range ls {
		if strings.HasPrefix(l, pre) {
			return i
		}
	}
	return -1
}

func matarVMM(pid int) {
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		waitGone(pid, 5*time.Second)
	}
}

func TestVZBootMandaRedAntesDeArrancarYReenviaDespues(t *testing.T) {
	m, logPath := managerVZ(t)
	id := "aa11bb22cc33dd44"
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	m.byID[id] = &api.Machine{ID: id, Name: "vz", State: api.StateCreated, Egress: "internet",
		Labels: map[string]string{api.LabelPorts: "9000"}}
	base := filepath.Join(m.root, "base.ext4")
	overlay := filepath.Join(m.dir(id), "overlay.ext4")
	for _, f := range []string{base, overlay} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pid, err := m.boot(ctx, id, 1, 256, 0, base, "", overlay, knet.Plan(1, id), nil, false)
	defer matarVMM(pid)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}

	ls := llamadas(t, logPath)
	red, start, fwd := indice(ls, "PUT /kling/network"), indice(ls, "PUT /actions"), indice(ls, "PUT /kling/forwards")
	if red < 0 || start < 0 || fwd < 0 || !(red < start && start < fwd) {
		t.Fatalf("orden de llamadas incorrecto (red=%d start=%d forwards=%d):\n%s", red, start, fwd, strings.Join(ls, "\n"))
	}
	if !strings.HasSuffix(ls[red], "egress=internet") {
		t.Fatalf("la política de salida no viajó: %q", ls[red])
	}
	if indice(ls, "PUT /boot-source") > red || indice(ls, "PUT /drives/rootfs") > red {
		t.Fatalf("la red va tras configurar la máquina:\n%s", strings.Join(ls, "\n"))
	}

	mc := m.byID[id]
	if got := mc.Addr(api.GuestPort); got != puertoFalso(api.GuestPort) {
		t.Fatalf("Addr(8080) = %q", got)
	}
	if got := mc.Addr(9000); got != puertoFalso(9000) {
		t.Fatalf("Addr(9000) = %q (los puertos de kling.ports también se reenvían)", got)
	}
	if mc.IP != "" {
		t.Fatalf("boot no toca la IP: %q", mc.IP)
	}
	if got := knet.Plan(1, id).NSIP; got != knet.GuestIP {
		t.Fatalf("en macOS la IP informativa es la del invitado, no %q", got)
	}

	// La memoria del VMM se le pregunta al ayudante.
	if n := memoriaVMM(pid, m.socket[id]); n != 321 {
		t.Fatalf("memoriaVMM = %d", n)
	}
	// Y el VMM se reconoce en la tabla de procesos, sin /proc.
	if got := m.liveVMs()[id]; got != pid {
		t.Fatalf("liveVMs no encuentra el VMM (pid %d): %v", pid, m.liveVMs())
	}
	m.byID[id].PID = pid
	if sock, ok := m.adopt(m.byID[id]); !ok || sock != m.dir(id)+"/fc.sock" {
		t.Fatalf("adopt = %q, %v", sock, ok)
	}
}

func TestVZThawMandaRedAntesDeCargarYReenviaDespues(t *testing.T) {
	m, logPath := managerVZ(t)
	id := "ee55ff66aa77bb88"
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"snap.file", "mem.file", "overlay.ext4"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := volcadoEnCurso(dir); err != nil {
		t.Fatal(err)
	}
	if err := sellarVolcado(dir); err != nil {
		t.Fatal(err)
	}
	m.byID[id] = &api.Machine{ID: id, Name: "vz-warm", State: api.StateWarm, Egress: "none",
		IP: knet.GuestIP, CreatedAt: time.Now()}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := m.Thaw(ctx, id)
	if out != nil {
		defer matarVMM(out.PID)
	}
	if err != nil {
		t.Fatalf("thaw: %v", err)
	}
	ls := llamadas(t, logPath)
	red, load, fwd := indice(ls, "PUT /kling/network"), indice(ls, "PUT /snapshot/load"), indice(ls, "PUT /kling/forwards")
	if red < 0 || load < 0 || fwd < 0 || !(red < load && load < fwd) {
		t.Fatalf("orden de llamadas incorrecto (red=%d load=%d forwards=%d):\n%s", red, load, fwd, strings.Join(ls, "\n"))
	}
	if out.State != api.StateRunning || out.Addr(api.GuestPort) != puertoFalso(api.GuestPort) {
		t.Fatalf("thaw devolvió %+v", out)
	}

	// Congelar la máquina borra sus reenvíos: eran del proceso que muere.
	m.mu.Lock()
	m.byID[id].Forwards = map[string]string{"8080": "127.0.0.1:1"}
	m.mu.Unlock()
	if _, err := m.Stop(id); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get(id); len(got.Forwards) != 0 {
		t.Fatalf("Stop dejó reenvíos: %v", got.Forwards)
	}
}

func TestVZCopiarDiscoClona(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(src, []byte("disco"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := copiarDisco(context.Background(), src, dst); err != nil {
		t.Fatalf("copiarDisco: %v: %s", err, out)
	}
	if b, _ := os.ReadFile(dst); string(b) != "disco" {
		t.Fatalf("copia = %q", b)
	}
	if out, err := perforarHuecos(context.Background(), dst); err != nil || out != nil {
		t.Fatal("perforarHuecos no hace nada en macOS")
	}
}

func TestVZSinJailerNiCgroupsNiPrivilegios(t *testing.T) {
	t.Setenv("KLING_JAILER", "1")
	if jailerEnabled() {
		t.Fatal("macOS no tiene jailer, aunque se pida")
	}
	if _, err := delegacionCgroups(); err == nil || !strings.Contains(err.Error(), "cgroups") {
		t.Fatalf("delegacionCgroups = %v", err)
	}
	if p, warn := privilegiosPlataforma("kindling"); p.Enabled || warn != "" {
		t.Fatal("en macOS no se bajan privilegios")
	}
	if n := maxParallelLaunch(); n != 4 {
		t.Fatalf("maxParallelLaunch = %d, quiero 4", n)
	}
	t.Setenv("KLING_MAX_PARALLEL_BOOT", "7")
	if n := maxParallelLaunch(); n != 7 {
		t.Fatalf("KLING_MAX_PARALLEL_BOOT no manda: %d", n)
	}
}

func TestVZFaltaE2fsDiceComoInstalarlo(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := e2fsCmd(context.Background(), "herramienta-que-no-existe").Run()
	if err == nil || !strings.Contains(err.Error(), "brew install e2fsprogs") {
		t.Fatalf("error = %v", err)
	}
}

func TestVZMemoriaHost(t *testing.T) {
	if memoriaFisicaMiB() < 1024 {
		t.Fatalf("hw.memsize = %d MiB", memoriaFisicaMiB())
	}
	if a, _ := memoriaHost(); a <= 0 {
		t.Fatalf("memoriaHost = %d", a)
	}
	// La admisión real no debe fallar en una máquina de desarrollo sana... salvo
	// que lo esté de verdad; basta con que no rompa.
	_ = checkPresionPlataforma()
}
