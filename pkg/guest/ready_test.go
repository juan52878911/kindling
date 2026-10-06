package guest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

func nuevoReadiness(t *testing.T) (*readiness, string) {
	t.Helper()
	dir := t.TempDir()
	r := &readiness{
		probePath: filepath.Join(dir, "ready"),
		hooksDir:  filepath.Join(dir, "post-restore.d"),
		run:       runWithTimeout,
		env:       []string{"PATH=/usr/bin:/bin"},
	}
	return r, dir
}

func script(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestReadySinSondaEsListo(t *testing.T) {
	r, _ := nuevoReadiness(t)
	st := r.check(context.Background())
	if !st.Ready || st.Probe || st.Declares() {
		t.Fatalf("sin sonda ni ganchos = %+v, quiero listo sin declarar nada", st)
	}
}

func TestReadySondaSeRecuerda(t *testing.T) {
	r, dir := nuevoReadiness(t)
	marca := filepath.Join(dir, "boot_completed")
	// Lista solo cuando existe la marca; cuenta cuántas veces se ejecuta.
	cuenta := filepath.Join(dir, "cuenta")
	script(t, r.probePath, `echo x >> `+cuenta+`; test -f `+marca+` || { echo "not yet"; exit 1; }`)

	st := r.check(context.Background())
	if st.Ready || !st.Probe || !strings.Contains(st.Detail, "not yet") {
		t.Fatalf("antes de la marca = %+v, quiero no listo con la salida de la sonda", st)
	}
	if err := os.WriteFile(marca, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if st = r.check(context.Background()); !st.Ready {
		t.Fatalf("con la marca = %+v, quiero listo", st)
	}
	// Ya listo: no se vuelve a ejecutar aunque la condición cambie.
	os.Remove(marca)
	if st = r.check(context.Background()); !st.Ready {
		t.Fatalf("listo tiene que recordarse: %+v", st)
	}
	b, _ := os.ReadFile(cuenta)
	if n := strings.Count(string(b), "x"); n != 2 {
		t.Errorf("la sonda corrió %d veces, quiero 2", n)
	}
}

func TestReadySondaNoEjecutableNoCuenta(t *testing.T) {
	r, _ := nuevoReadiness(t)
	if err := os.WriteFile(r.probePath, []byte("#!/bin/sh\nexit 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st := r.check(context.Background()); !st.Ready || st.Probe {
		t.Fatalf("una sonda sin permiso de ejecución no es sonda: %+v", st)
	}
}

func TestReadySondaConPlazo(t *testing.T) {
	r, _ := nuevoReadiness(t)
	script(t, r.probePath, "sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	st := r.check(ctx)
	if st.Ready || time.Since(t0) > 5*time.Second {
		t.Fatalf("sonda colgada: %+v en %s", st, time.Since(t0))
	}
	if !strings.Contains(st.Detail, "timed out") {
		t.Errorf("Detail = %q, quiero que diga que agotó el plazo", st.Detail)
	}
}

func TestHooksEnOrdenYEstado(t *testing.T) {
	r, dir := nuevoReadiness(t)
	salida := filepath.Join(dir, "orden")
	script(t, filepath.Join(r.hooksDir, "20-segundo"), `echo "2 $KLING_RESTORE" >> `+salida)
	script(t, filepath.Join(r.hooksDir, "10-primero"), `echo "1 $KLING_RESTORE" >> `+salida)
	script(t, filepath.Join(r.hooksDir, ".oculto"), `echo oculto >> `+salida)
	script(t, filepath.Join(r.hooksDir, "30-viejo.disabled"), `echo off >> `+salida)
	// Uno sin permiso de ejecución tampoco corre.
	os.WriteFile(filepath.Join(r.hooksDir, "40-sin-x"), []byte("#!/bin/sh\necho sinx >> "+salida+"\n"), 0o644)

	hecho := make(chan struct{})
	if !r.startHooks(api.ResyncInstance, func() { close(hecho) }) {
		t.Fatal("startHooks no arrancó")
	}
	<-hecho
	if !r.snapshot().HasHooks {
		t.Error("snapshot tiene que decir que la imagen declara ganchos")
	}
	b, _ := os.ReadFile(salida)
	if got := string(b); got != "1 instance\n2 instance\n" {
		t.Fatalf("salida de los ganchos = %q", got)
	}
	st := r.check(context.Background())
	if !st.Ready || st.Hooks != api.HooksDone {
		t.Fatalf("tras los ganchos = %+v", st)
	}
}

func TestHooksFalloYMientrasCorren(t *testing.T) {
	r, dir := nuevoReadiness(t)
	sigue := filepath.Join(dir, "sigue")
	script(t, filepath.Join(r.hooksDir, "10-espera"), `while [ ! -f `+sigue+` ]; do sleep 0.05; done`)
	script(t, filepath.Join(r.hooksDir, "20-falla"), `echo "no identity in MMDS" >&2; exit 3`)
	script(t, filepath.Join(r.hooksDir, "30-nunca"), `touch `+filepath.Join(dir, "nunca"))

	hecho := make(chan struct{})
	r.startHooks(api.ResyncThaw, func() { close(hecho) })
	if st := r.check(context.Background()); st.Ready || st.Hooks != api.HooksRunning {
		t.Fatalf("con ganchos en marcha = %+v, quiero no listo", st)
	}
	if r.startHooks(api.ResyncThaw, nil) {
		t.Fatal("una segunda tanda no debe solaparse con la primera")
	}
	os.WriteFile(sigue, nil, 0o644)
	<-hecho
	st := r.check(context.Background())
	if st.Ready || st.Hooks != api.HooksFailed || !strings.Contains(st.Detail, "20-falla") ||
		!strings.Contains(st.Detail, "no identity in MMDS") {
		t.Fatalf("tras el fallo = %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "nunca")); err == nil {
		t.Error("tras un gancho fallido no deben correr los siguientes")
	}
	// Relanzarlos (POST /hooks) con el fallo arreglado deja listo.
	script(t, filepath.Join(r.hooksDir, "20-falla"), `exit 0`)
	otra := make(chan struct{})
	if !r.startHooks("manual", func() { close(otra) }) {
		t.Fatal("relanzar tras un fallo tiene que poder")
	}
	<-otra
	if st := r.check(context.Background()); !st.Ready {
		t.Fatalf("tras relanzar = %+v", st)
	}
}

func TestReadyHandlerCodigos(t *testing.T) {
	r, _ := nuevoReadiness(t)
	var fallar atomic.Bool
	fallar.Store(true)
	r.run = func(ctx context.Context, path string, env []string) (string, error) {
		if fallar.Load() {
			return "booting", os.ErrNotExist
		}
		return "", nil
	}
	script(t, r.probePath, "exit 0") // existe y es ejecutable; la ejecución es la falsa

	srv := httptest.NewServer(http.HandlerFunc(r.readyHandler))
	defer srv.Close()
	get := func() (int, api.GuestReady) {
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var st api.GuestReady
		json.NewDecoder(resp.Body).Decode(&st)
		return resp.StatusCode, st
	}
	if code, st := get(); code != http.StatusServiceUnavailable || st.Ready || !st.Probe {
		t.Fatalf("no listo = %d %+v", code, st)
	}
	fallar.Store(false)
	if code, st := get(); code != http.StatusOK || !st.Ready {
		t.Fatalf("listo = %d %+v", code, st)
	}
	resp, _ := http.Post(srv.URL, "text/plain", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /ready = %d", resp.StatusCode)
	}
}

func TestHooksHandler(t *testing.T) {
	r, _ := nuevoReadiness(t)
	srv := httptest.NewServer(http.HandlerFunc(r.hooksHandler))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"?restore=$(reboot)", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /hooks = %d", resp.StatusCode)
	}
	if restoreKind("$(reboot)", "manual") != "manual" {
		t.Error("KLING_RESTORE tiene que ser un valor conocido")
	}
}

func TestReadyYHooksSonDeControl(t *testing.T) {
	for _, p := range []string{api.GuestReadyPath, api.GuestHooksPath, api.GuestMemInfoPath} {
		if !IsControlPath(p) {
			t.Errorf("%s tiene que ser ruta de control (el gateway no la reenvía)", p)
		}
	}
}

func TestParseMeminfo(t *testing.T) {
	mi, ok := parseMeminfo(strings.NewReader("MemTotal:        1015808 kB\nMemFree:  2048 kB\nMemAvailable:      4096 kB\n"))
	if !ok || mi.TotalMiB != 992 || mi.AvailableMiB != 4 {
		t.Fatalf("meminfo = %+v %v", mi, ok)
	}
	if _, ok := parseMeminfo(strings.NewReader("MemTotal: 1 kB\n")); ok {
		t.Error("sin MemAvailable no vale")
	}
}

// La sonda corre con el USER del servicio (como el HEALTHCHECK de Docker); si
// ese usuario no existe no corre como root por accidente: falla.
func TestSondaConUsuarioDelServicio(t *testing.T) {
	dir := t.TempDir()
	marca := filepath.Join(dir, "corrio")
	sonda := filepath.Join(dir, "ready")
	script(t, sonda, "touch "+marca)
	if err := os.MkdirAll(filepath.Join(dir, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "etc/passwd"), []byte("root:x:0:0::/root:/bin/sh\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "etc/group"), []byte("root:x:0:\n"), 0o644)

	serviceState.mu.Lock()
	serviceState.svc = &Service{spec: api.ServiceSpec{Argv: []string{"x"}, User: "postgres"}, root: dir}
	serviceState.mu.Unlock()
	defer func() { serviceState.mu.Lock(); serviceState.svc = nil; serviceState.mu.Unlock() }()

	if _, err := runProbeAsService(context.Background(), sonda, []string{"PATH=/usr/bin:/bin"}); err == nil {
		t.Fatal("la sonda corrió con un USER que no existe")
	}
	if _, err := os.Stat(marca); err == nil {
		t.Fatal("la sonda se ejecutó como root en vez de con el usuario del servicio")
	}

	// Sin servicio, la sonda corre como siempre.
	serviceState.mu.Lock()
	serviceState.svc = nil
	serviceState.mu.Unlock()
	if out, err := runProbeAsService(context.Background(), sonda, []string{"PATH=/usr/bin:/bin"}); err != nil {
		t.Fatalf("sin servicio: %v: %s", err, out)
	}
	if _, err := os.Stat(marca); err != nil {
		t.Fatal("sin servicio la sonda no corrió")
	}
}
