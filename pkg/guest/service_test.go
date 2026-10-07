package guest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestLookupUser(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("root:x:0:0:root:/root:/bin/sh\npostgres:x:70:70::/var/lib/postgresql:/bin/sh\n"), 0o644)
	os.WriteFile(filepath.Join(root, "etc/group"), []byte("root:x:0:\npostgres:x:70:\nssl:x:101:postgres,other\n"), 0o644)
	for spec, want := range map[string]userInfo{
		"postgres":     {uid: 70, gid: 70, groups: []uint32{70, 101}, home: "/var/lib/postgresql"},
		"70":           {uid: 70, gid: 70, groups: []uint32{70, 101}, home: "/var/lib/postgresql"},
		"70:0":         {uid: 70, gid: 0, groups: []uint32{0, 101}, home: "/var/lib/postgresql"},
		"postgres:ssl": {uid: 70, gid: 101, groups: []uint32{101}, home: "/var/lib/postgresql"},
		"1000:1000":    {uid: 1000, gid: 1000, groups: []uint32{1000}},
		// Sin entrada en /etc/passwd, grupo 0, como Docker.
		"1000":   {uid: 1000, gid: 0, groups: []uint32{0}},
		"1000:5": {uid: 1000, gid: 5, groups: []uint32{5}},
	} {
		got, err := lookupUser(root, spec)
		if err != nil || got.uid != want.uid || got.gid != want.gid || got.home != want.home || !sameGroups(got.groups, want.groups) {
			t.Errorf("lookupUser(%q) = %+v, %v; want %+v", spec, got, err, want)
		}
	}
	for _, bad := range []string{"nadie", "postgres:nogroup"} {
		if _, err := lookupUser(root, bad); err == nil {
			t.Errorf("lookupUser(%q) accepted", bad)
		}
	}
}

func sameGroups(a, b []uint32) bool {
	m := map[uint32]bool{}
	for _, g := range a {
		m[g] = true
	}
	if len(m) != len(b) {
		return false
	}
	for _, g := range b {
		if !m[g] {
			return false
		}
	}
	return true
}

func TestStopSignal(t *testing.T) {
	for s, want := range map[string]syscall.Signal{"": syscall.SIGTERM, "SIGINT": syscall.SIGINT, "quit": syscall.SIGQUIT,
		"9": syscall.SIGKILL, "SIGNOPE": syscall.SIGTERM, "SIGRTMIN+3": syscall.Signal(37), "SIGABRT": syscall.Signal(6)} {
		if got := stopSignal(api.ServiceSpec{StopSignal: s}); got != want {
			t.Errorf("stopSignal(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestLookPathEnv(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "docker-entrypoint.sh"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "noexec"), []byte("x"), 0o644)
	env := []string{"PATH=/nonexistent:" + dir}
	if p, err := lookPathEnv("docker-entrypoint.sh", env, ""); err != nil || p != filepath.Join(dir, "docker-entrypoint.sh") {
		t.Fatalf("%s %v", p, err)
	}
	if _, err := lookPathEnv("noexec", env, ""); err == nil {
		t.Fatal("non-executable file accepted")
	}
	if p, err := lookPathEnv("./docker-entrypoint.sh", nil, dir); err != nil || p != filepath.Join(dir, "docker-entrypoint.sh") {
		t.Fatalf("relative to the workdir: %s %v", p, err)
	}
}

// newTestService es un servicio con el log en un temporal y sin consola.
func newTestService(t *testing.T, spec api.ServiceSpec) *Service {
	t.Helper()
	if err := checkServiceSpec(spec); err != nil {
		t.Fatal(err)
	}
	// Start lo deja en el estado global (el de /service y las capacidades).
	t.Cleanup(resetServiceState)
	return &Service{spec: spec, root: "/", logPath: filepath.Join(t.TempDir(), "svc.log")}
}

func resetServiceState() {
	serviceState.mu.Lock()
	serviceState.svc = nil
	serviceState.mu.Unlock()
}

func waitUntil(t *testing.T, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestServiceRestartsAndLogs(t *testing.T) {
	dir := t.TempDir()
	s := newTestService(t, api.ServiceSpec{Argv: []string{"sh", "-c", "pwd; echo \"v=$V\"; exit 3"}, WorkingDir: dir})
	s.Start([]string{"PATH=/usr/bin:/bin", "V=7"})
	defer s.Stop()
	waitUntil(t, "a restart", func() bool { return s.Status(0).Starts >= 2 })
	st := s.Status(1 << 10)
	if st.LastExit != "exit status 3" || !strings.Contains(st.Log, "v=7") || !strings.Contains(st.Log, "restarting in") {
		t.Fatalf("%+v", st)
	}
	real, _ := filepath.EvalSymlinks(dir)
	if !strings.Contains(st.Log, dir) && !strings.Contains(st.Log, real) {
		t.Fatalf("not run in its working dir: %q", st.Log)
	}
}

func TestServiceRestartPolicy(t *testing.T) {
	s := newTestService(t, api.ServiceSpec{Argv: []string{"true"}, Restart: api.RestartOnFailure})
	s.Start([]string{"PATH=/usr/bin:/bin"})
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		t.Fatal("on-failure restarted a clean exit")
	}
	if st := s.Status(0); st.Starts != 1 || st.Running {
		t.Fatalf("%+v", st)
	}

	// on-failure sí relanza una salida con error.
	s = newTestService(t, api.ServiceSpec{Argv: []string{"false"}, Restart: api.RestartOnFailure})
	s.Start([]string{"PATH=/usr/bin:/bin"})
	waitUntil(t, "a restart after a failure", func() bool { return s.Status(0).Starts >= 2 })
	s.Stop()

	// no: ni con error.
	s = newTestService(t, api.ServiceSpec{Argv: []string{"false"}, Restart: api.RestartNo})
	s.Start([]string{"PATH=/usr/bin:/bin"})
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		t.Fatal("no restarted a failure")
	}
	if st := s.Status(0); st.Starts != 1 || st.LastExit != "exit status 1" {
		t.Fatalf("%+v", st)
	}
}

func TestServiceStopSignalAndKill(t *testing.T) {
	// Con SIGINT sale con 0; el bucle no lo relanza.
	s := newTestService(t, api.ServiceSpec{Argv: []string{"sh", "-c", "trap 'echo got-int; exit 0' INT; echo up; while :; do sleep 0.05; done"}, StopSignal: "SIGINT"})
	s.Start([]string{"PATH=/usr/bin:/bin"})
	waitUntil(t, "the service", func() bool { return strings.Contains(s.Status(1<<10).Log, "up") })
	s.Stop()
	if st := s.Status(1 << 10); st.Running || !strings.Contains(st.Log, "got-int") || st.Starts != 1 {
		t.Fatalf("%+v", st)
	}

	// Uno que ignora la señal se lleva SIGKILL al acabar el plazo.
	s = newTestService(t, api.ServiceSpec{Argv: []string{"sh", "-c", "trap '' TERM; echo up; while :; do sleep 0.05; done"}, StopTimeoutSeconds: 1})
	s.Start([]string{"PATH=/usr/bin:/bin"})
	waitUntil(t, "the service", func() bool { return strings.Contains(s.Status(1<<10).Log, "up") })
	t0 := time.Now()
	s.Stop()
	if d := time.Since(t0); d < time.Second || d > 8*time.Second {
		t.Fatalf("stop took %s", d)
	}
	if st := s.Status(0); st.Running || !strings.Contains(st.LastExit, "killed") {
		t.Fatalf("%+v", st)
	}
}

func TestServiceNotFound(t *testing.T) {
	s := newTestService(t, api.ServiceSpec{Argv: []string{"no-such-binary"}, Restart: api.RestartNo})
	s.Start([]string{"PATH=/usr/bin:/bin"})
	<-s.done
	if st := s.Status(0); st.Starts != 0 || !strings.Contains(st.Error, "not found") {
		t.Fatalf("%+v", st)
	}
}

func TestLoadServiceRejectsBadSpecs(t *testing.T) {
	dir := t.TempDir()
	if s, err := LoadService(filepath.Join(dir, "none.json")); s != nil || err != nil {
		t.Fatalf("missing file: %v %v", s, err)
	}
	for _, bad := range []string{`{}`, `{"argv":["x"],"restart":"sometimes"}`, `nope`} {
		p := filepath.Join(dir, "s.json")
		os.WriteFile(p, []byte(bad), 0o644)
		if _, err := LoadService(p); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestRotatingLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "l")
	l := &rotatingLog{path: p, max: 10}
	l.Write([]byte("12345678"))
	l.Write([]byte("abcdef"))
	l.Close()
	a, _ := os.ReadFile(p + ".1")
	b, _ := os.ReadFile(p)
	if string(a) != "12345678" || string(b) != "abcdef" {
		t.Fatalf("%q %q", a, b)
	}
}

func TestServiceStopHandlerAndCaps(t *testing.T) {
	resetServiceState()
	a := &Agent{}
	if slices.Contains(a.Caps(), api.GuestCapService) {
		t.Fatal("service announced without a service")
	}
	s := newTestService(t, api.ServiceSpec{Argv: []string{"sh", "-c", "trap 'echo bye; exit 0' TERM; echo up; while :; do sleep 0.05; done"}})
	s.Start([]string{"PATH=/usr/bin:/bin"})
	if !slices.Contains(a.Caps(), api.GuestCapService) {
		t.Fatal("service not announced")
	}
	waitUntil(t, "the service", func() bool { return strings.Contains(s.Status(1<<10).Log, "up") })

	// Dos paradas a la vez (el daemon y el apagado del agente): las dos
	// vuelven cuando el servicio ya salió.
	other := make(chan struct{})
	go func() { StopService(); close(other) }()
	rec := httptest.NewRecorder()
	ServiceStopHandler()(rec, httptest.NewRequest(http.MethodPost, api.GuestServiceStopPath, nil))
	<-other
	var st api.GuestService
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || rec.Code != 200 || st.Running || !st.Declared {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(s.Status(1<<10).Log, "bye") {
		t.Fatal("not stopped with its signal")
	}
	rec = httptest.NewRecorder()
	ServiceStopHandler()(rec, httptest.NewRequest(http.MethodGet, api.GuestServiceStopPath, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /service/stop: %d", rec.Code)
	}
}

// Un STOPSIGNAL que no se entiende no deja la imagen sin servicio: se carga
// con SIGTERM. Antes LoadService fallaba y el servicio no arrancaba.
func TestLoadServiceUnknownSignalUsesTerm(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	os.WriteFile(p, []byte(`{"argv":["x"],"stop_signal":"SIGNOPE"}`), 0o644)
	s, err := LoadService(p)
	if err != nil || s == nil {
		t.Fatalf("LoadService: %v %v", s, err)
	}
	if s.spec.StopSignal != "" || stopSignal(s.spec) != syscall.SIGTERM {
		t.Fatalf("stop signal %q", s.spec.StopSignal)
	}
	os.WriteFile(p, []byte(`{"argv":["x"],"stop_signal":"SIGRTMIN+3"}`), 0o644)
	if s, err := LoadService(p); err != nil || s.spec.StopSignal != "SIGRTMIN+3" {
		t.Fatalf("SIGRTMIN+3: %v %v", s, err)
	}
}
