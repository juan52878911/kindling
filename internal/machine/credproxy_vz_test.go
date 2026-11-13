//go:build darwin

package machine

// El proxy de credenciales aparte con un VMM y un kling-credproxy de mentira
// (este binario de test relanzado, ver TestMain en vz_test.go): a quién van
// las claves, a quién los dominios, que se lanza una vez por VMM, que muere
// con él y que no se le da nada a un socket que no es suyo.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// envFakeCPRechaza, si está, hace que el kling-credproxy falso no conteste a
// nadie (como uno que solo admite a otro ejecutable del daemon).
const envFakeCPRechaza = "KLING_FAKE_VZ_CP_RECHAZA"

// servirCredproxyFalso hace de kling-credproxy: escucha en --dir, apunta lo
// que recibe (nunca una clave) en el mismo fichero que el VMM falso, con
// "CP " delante, y sale cuando muere --vmm-pid.
func servirCredproxyFalso(args []string, logPath string) {
	var vmm int
	var dir string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--vmm-pid":
			vmm, _ = strconv.Atoi(args[i+1])
		case "--dir":
			dir = args[i+1]
		}
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		os.Exit(1)
	}
	var mu sync.Mutex
	apuntar := func(l string) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintln(f, "CP "+l)
		_ = f.Sync()
	}
	if err := os.Chdir(dir); err != nil {
		os.Exit(1)
	}
	_ = os.Remove(credproxy.SplitAPISock) // el de uno muerto, como el de verdad
	ln, err := net.Listen("unix", credproxy.SplitAPISock)
	if err != nil {
		apuntar("ERROR " + err.Error())
		os.Exit(1)
	}
	apuntar(fmt.Sprintf("START pid=%d vmm=%d daemon=%t vmmexe=%t", os.Getpid(), vmm,
		os.Getenv("KLING_CREDPROXY_DAEMON") != "", os.Getenv("KLING_CREDPROXY_VMM") != ""))
	go func() {
		for syscall.Kill(vmm, 0) == nil {
			time.Sleep(20 * time.Millisecond)
		}
		apuntar("EXIT")
		os.Exit(0)
	}()
	if os.Getenv(envFakeCPRechaza) != "" {
		// Como uno lanzado por otro daemon: acepta y cierra sin contestar.
		for {
			c, err := ln.Accept()
			if err != nil {
				os.Exit(0)
			}
			c.Close()
		}
	}
	_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		linea := r.Method + " " + r.URL.Path
		switch r.URL.Path {
		case "/kling/info":
			apuntar(linea)
			_ = json.NewEncoder(w).Encode(map[string]any{"backend": "credproxy",
				"credential_kinds": []string{"http", "http-places", "postgres", "postgres-upstream", "mysql", "sigv4"}})
			return
		case "/kling/network":
			var n struct{ Egress string }
			_ = json.Unmarshal(body, &n)
			apuntar(linea + " egress=" + n.Egress)
			w.WriteHeader(http.StatusNoContent)
			return
		case "/kling/credentials":
			var c struct {
				Credentials []struct{ Domain, Secret string } `json:"credentials"`
			}
			_ = json.Unmarshal(body, &c)
			linea += fmt.Sprintf(" n=%d", len(c.Credentials))
			var doms []string
			for _, cr := range c.Credentials {
				linea += " " + cr.Domain
				if cr.Secret != "" {
					linea += "+secret"
				}
				doms = append(doms, cr.Domain)
			}
			apuntar(linea)
			_ = json.NewEncoder(w).Encode(map[string]any{"domains": doms})
			return
		}
		apuntar(linea)
		w.WriteHeader(http.StatusNotFound)
	}))
}

// credproxyJuntoAlVMM pone una copia de este binario de test como
// kling-credproxy junto a él (el VMM falso), donde el daemon lo busca.
func credproxyJuntoAlVMM(t *testing.T) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(filepath.Dir(exe), credproxy.SplitBinary)
	if out, err := exec.Command("/bin/cp", "-c", exe, dst).CombinedOutput(); err != nil {
		t.Fatalf("cp -c: %v %s", err, out)
	}
	t.Cleanup(func() { os.Remove(dst) })
}

// arrancarVZ arranca la máquina id con el VMM falso y devuelve su pid.
func arrancarVZ(t *testing.T, m *Manager, id, egress string) int {
	t.Helper()
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m.byID[id] = &api.Machine{ID: id, Name: "vz-" + id[:4], State: api.StateCreated, Egress: egress}
	base := filepath.Join(m.root, "base.ext4")
	overlay := filepath.Join(dir, "overlay.ext4")
	for _, f := range []string{base, overlay} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pid, err := m.boot(ctx, id, 1, 256, 0, base, "", overlay, knet.Plan(1, id), nil, false, false, nil, 0)
	t.Cleanup(func() { matarVMM(pid) })
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	return pid
}

func lineasCon(ls []string, pre string) []string {
	var out []string
	for _, l := range ls {
		if strings.HasPrefix(l, pre) {
			out = append(out, l)
		}
	}
	return out
}

// Con un VMM que anuncia credproxy-split, las claves van al proxy aparte
// (con el modo de salida antes) y el VMM recibe solo los dominios; una
// segunda entrega usa el mismo proxy, y el proxy muere con su VMM. Con la
// ruta larga de una raíz bajo "Application Support": el socket del proxy no
// cabe en sun_path y se conecta por el enlace corto.
func TestCredproxyAparte(t *testing.T) {
	t.Setenv(envFakeVZKinds, "http,http-places,postgres,"+credproxy.CapSplit)
	credproxyJuntoAlVMM(t)
	m, logPath := managerVZ(t, true)
	id := "cc11bb22cc33dd44"
	vmm := arrancarVZ(t, m, id, "allowlist")
	c := fc.New(m.socket[id])
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	creds := []credproxy.Credential{
		{Env: "KEY", Domain: "api.example.com", Placeholder: "kling-cred-bb", Secret: "sk-clave-de-prueba"},
		{Env: "PG", Domain: "db.example.com", Placeholder: "kling-cred-cc", Secret: "pw-clave-de-prueba", Kind: credproxy.KindPostgres, User: "app", Database: "app"},
	}
	if err := registrarCredencialesPlataforma(ctx, c, nil, creds, m.credAuditPath(id), nil, "allowlist"); err != nil {
		t.Fatal(err)
	}
	ls := llamadas(t, logPath)
	if i := indice(ls, "PUT /kling/credentials"); i >= 0 {
		t.Fatalf("the VMM got the credentials: %q", ls[i])
	}
	i := indice(ls, "PUT /kling/credproxy")
	if i < 0 {
		t.Fatalf("the VMM was not pointed at the proxy: %q", ls)
	}
	cp := lineasCon(ls, "CP ")
	want := []string{"CP START", "CP GET /kling/info", "CP PUT /kling/network egress=allowlist", "CP PUT /kling/credentials n=2 api.example.com+secret db.example.com+secret"}
	if len(cp) < len(want) {
		t.Fatalf("proxy calls: %q", cp)
	}
	for j, w := range want {
		if !strings.HasPrefix(cp[j], w) {
			t.Fatalf("proxy call %d: %q, want %q (all: %q)", j, cp[j], w, cp)
		}
	}
	if !strings.Contains(cp[0], "vmm="+strconv.Itoa(vmm)+" daemon=true vmmexe=true") {
		t.Fatalf("proxy start: %q", cp[0])
	}
	if fi, err := os.Stat(filepath.Join(m.dir(id), credproxy.SplitDir)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("proxy directory: %v %v", fi, err)
	}
	// Otra entrega: el mismo proxy, sin lanzar otro.
	if err := registrarCredencialesPlataforma(ctx, c, nil, creds[:1], m.credAuditPath(id), nil, "allowlist"); err != nil {
		t.Fatal(err)
	}
	ls = llamadas(t, logPath)
	if n := len(lineasCon(ls, "CP START")); n != 1 {
		t.Fatalf("%d proxies started, want 1: %q", n, ls)
	}
	// Muere el VMM: el proxy con él.
	matarVMM(vmm)
	limite := time.Now().Add(5 * time.Second)
	for len(lineasCon(llamadas(t, logPath), "CP EXIT")) == 0 {
		if time.Now().After(limite) {
			t.Fatal("the proxy outlived its VMM")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Un proxy que no contesta a este daemon (lo lanzó uno anterior desde otra
// ruta, tras una actualización) se cambia por uno nuevo al entregar.
func TestCredproxyQueNoContestaSeCambia(t *testing.T) {
	t.Setenv(envFakeVZKinds, "http,http-places,"+credproxy.CapSplit)
	credproxyJuntoAlVMM(t)
	m, logPath := managerVZ(t)
	id := "ab11bb22cc33dd44"
	arrancarVZ(t, m, id, "allowlist")
	creds := []credproxy.Credential{{Env: "KEY", Domain: "api.example.com", Placeholder: "kling-cred-bb", Secret: "sk-clave-de-prueba"}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	t.Setenv(envFakeCPRechaza, "1")
	if err := registrarCredencialesPlataforma(ctx, fc.New(m.socket[id]), nil, creds, m.credAuditPath(id), nil, "allowlist"); err == nil {
		t.Fatal("a proxy that does not answer took the credentials")
	}
	os.Unsetenv(envFakeCPRechaza)
	if err := registrarCredencialesPlataforma(ctx, fc.New(m.socket[id]), nil, creds, m.credAuditPath(id), nil, "allowlist"); err != nil {
		t.Fatal(err)
	}
	ls := llamadas(t, logPath)
	if n := len(lineasCon(ls, "CP START")); n != 2 {
		t.Fatalf("%d proxies started, want 2: %q", n, ls)
	}
	if len(lineasCon(ls, "CP PUT /kling/credentials n=1 api.example.com+secret")) != 1 {
		t.Fatalf("the new proxy did not get the credentials: %q", ls)
	}
}

// El broker reconoce al proxy aparte de una máquina por su ejecutable (junto
// al VMM) y su línea de órdenes, y a nadie más.
func TestBrokerReconoceAlCredproxy(t *testing.T) {
	t.Setenv(envFakeVZKinds, "http,http-places,"+credproxy.CapSplit)
	credproxyJuntoAlVMM(t)
	m, _ := managerVZ(t)
	id := "ff11bb22cc33dd44"
	vmm := arrancarVZ(t, m, id, "allowlist")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := registrarCredencialesPlataforma(ctx, fc.New(m.socket[id]), nil, []credproxy.Credential{
		{Env: "KEY", Domain: "api.example.com", Placeholder: "kling-cred-bb", Secret: "sk-clave-de-prueba"},
	}, m.credAuditPath(id), nil, "allowlist"); err != nil {
		t.Fatal(err)
	}
	conn, err := fc.DialUnix(ctx, filepath.Join(m.dir(id), credproxy.SplitDir, credproxy.SplitAPISock))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := pidDelOtro(conn.(*net.UnixConn))
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	gotVMM, dir, err := m.esCredproxy(pid)
	if err != nil || gotVMM != vmm || dir != filepath.Join(m.dir(id), credproxy.SplitDir) {
		t.Fatalf("esCredproxy(%d) = %d %q %v, want %d", pid, gotVMM, dir, err, vmm)
	}
	if _, _, err := m.esCredproxy(os.Getpid()); err == nil {
		t.Fatal("this test process was taken for the credential proxy")
	}
	if _, _, err := m.esCredproxy(vmm); err == nil {
		t.Fatal("the VMM was taken for the credential proxy")
	}
}

// Un socket del proxy que atiende otro programa (aquí, este mismo proceso, que
// no es el kling-credproxy junto al VMM) no recibe nada, y lo dice.
func TestCredproxyAjenoNoRecibeLasClaves(t *testing.T) {
	t.Setenv(envFakeVZKinds, "http,http-places,"+credproxy.CapSplit)
	credproxyJuntoAlVMM(t)
	m, logPath := managerVZ(t)
	id := "dd11bb22cc33dd44"
	arrancarVZ(t, m, id, "allowlist")
	cpdir := filepath.Join(m.dir(id), credproxy.SplitDir)
	if err := os.Mkdir(cpdir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(cpdir, credproxy.SplitAPISock))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var recibidas int
	var mu sync.Mutex
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		recibidas++
		mu.Unlock()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = registrarCredencialesPlataforma(ctx, fc.New(m.socket[id]), nil, []credproxy.Credential{
		{Env: "KEY", Domain: "api.example.com", Placeholder: "kling-cred-bb", Secret: "sk-clave-de-prueba"},
	}, m.credAuditPath(id), nil, "allowlist")
	if err == nil || !strings.Contains(err.Error(), "held by another program") {
		t.Fatalf("want a refusal, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if recibidas != 0 {
		t.Fatalf("the impostor got %d requests", recibidas)
	}
	ls := llamadas(t, logPath)
	if indice(ls, "PUT /kling/credentials") >= 0 || indice(ls, "PUT /kling/credproxy") >= 0 {
		t.Fatalf("something was handed over: %q", ls)
	}
}

// Sin credproxy-split (un kling-vz anterior), como siempre: las claves al VMM
// y ningún proxy aparte.
func TestCredproxyNoConVMMAnterior(t *testing.T) {
	t.Setenv(envFakeVZKinds, "http,http-places")
	m, logPath := managerVZ(t)
	id := "ee11bb22cc33dd44"
	arrancarVZ(t, m, id, "allowlist")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := registrarCredencialesPlataforma(ctx, fc.New(m.socket[id]), nil, []credproxy.Credential{
		{Env: "KEY", Domain: "api.example.com", Placeholder: "kling-cred-bb", Secret: "sk-clave-de-prueba"},
	}, m.credAuditPath(id), nil, "allowlist"); err != nil {
		t.Fatal(err)
	}
	ls := llamadas(t, logPath)
	if i := indice(ls, "PUT /kling/credentials"); i < 0 || !strings.Contains(ls[i], "+secret") {
		t.Fatalf("the old VMM did not get the credentials: %q", ls)
	}
	if len(lineasCon(ls, "CP ")) != 0 {
		t.Fatalf("a proxy was started: %q", ls)
	}
	if _, err := os.Stat(filepath.Join(m.dir(id), credproxy.SplitDir)); err == nil {
		t.Fatal("the proxy directory was created")
	}
}
