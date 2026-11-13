//go:build darwin

package main

// El proceso entero, en un hijo (este mismo binario de test): se encierra en
// su perfil de verdad, solo admite al «daemon» y al «VMM» que le dicen (aquí,
// este binario de test), registra en su directorio y sale cuando muere su VMM.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/credproxy"
)

const envHijo = "KLING_CREDPROXY_TEST_ARGS"

func TestMain(m *testing.M) {
	if a := os.Getenv(envHijo); a != "" {
		os.Args = append([]string{"kling-credproxy"}, strings.Split(a, "\x1f")...)
		os.Exit(run())
	}
	os.Exit(m.Run())
}

type hijo struct {
	dir string
	cmd *exec.Cmd
	vmm *exec.Cmd
	fin chan struct{}
}

// lanzar arranca el proxy con un «VMM» (un sleep) y espera a sus sockets.
func lanzar(t *testing.T, conSandbox bool) *hijo {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "kcp") // corto: sun_path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	dir := filepath.Join(base, credproxy.SplitDir)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	vmm := exec.Command("/bin/sleep", "60")
	if err := vmm.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { vmm.Process.Kill(); vmm.Wait() })
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(),
		envHijo+"="+strings.Join([]string{"--vmm-pid", strconv.Itoa(vmm.Process.Pid), "--dir", dir}, "\x1f"),
		"KLING_CREDPROXY_DAEMON="+exe, "KLING_CREDPROXY_VMM="+exe, "KLING_VZ_BROKER=")
	if !conSandbox {
		cmd.Env = append(cmd.Env, "KLING_VZ_NO_SANDBOX=1")
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &hijo{dir: dir, cmd: cmd, vmm: vmm, fin: make(chan struct{})}
	go func() { cmd.Wait(); close(h.fin) }()
	t.Cleanup(func() { cmd.Process.Kill(); <-h.fin })
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(filepath.Join(dir, credproxy.SplitFwdSock)); err == nil {
			if c, err := net.Dial("unix", filepath.Join(dir, credproxy.SplitAPISock)); err == nil {
				c.Close()
				return h
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the proxy did not open its sockets")
	return nil
}

func (h *hijo) api(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(h.dir, credproxy.SplitAPISock))
	}}, Timeout: 5 * time.Second}
	req, _ := http.NewRequest(method, "http://credproxy"+path, strings.NewReader(body))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// reenviar manda una petición HTTP por el socket de reenvío, como el VMM.
func (h *hijo) reenviar(t *testing.T, host string) int {
	t.Helper()
	c, err := net.Dial("unix", filepath.Join(h.dir, credproxy.SplitFwdSock))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cab, _ := credproxy.SplitHeader(80)
	c.Write(cab[:])
	req, _ := http.NewRequest("GET", "http://"+host+"/v1/x", nil)
	req.Header.Set("Authorization", "Bearer kling-cred-0123456789abcdef")
	if err := req.Write(c); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), req)
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestProcesoEntero(t *testing.T) {
	h := lanzar(t, os.Getenv("KLING_CREDPROXY_TEST_NOSB") == "")
	code, out := h.api(t, "GET", "/kling/info", "")
	var info struct {
		Backend string   `json:"backend"`
		Kinds   []string `json:"credential_kinds"`
	}
	if code != 200 || json.Unmarshal([]byte(out), &info) != nil || info.Backend != "credproxy" || !strings.Contains(out, `"sigv4"`) {
		t.Fatalf("info: %d %s", code, out)
	}
	const cred = `{"credentials":[{"env":"K","domain":"api.example.com","placeholder":"kling-cred-0123456789abcdef","secret":"sk-1-clave-de-prueba"}]}`
	// Sin allowlist (el modo lo dice el daemon, none por defecto) no las acepta.
	if code, out := h.api(t, "PUT", "/kling/credentials", cred); code != 400 || !strings.Contains(out, "allowlist") {
		t.Fatalf("credentials in none: %d %s", code, out)
	}
	if code, out := h.api(t, "PUT", "/kling/network", `{"egress":"allowlist"}`); code != 204 {
		t.Fatalf("network: %d %s", code, out)
	}
	if code, out := h.api(t, "PUT", "/kling/credentials", cred); code != 200 || !strings.Contains(out, `"domains":["api.example.com"]`) {
		t.Fatalf("credentials: %d %s", code, out)
	}
	// Otro dominio: 403 del proxy, sin salir, y queda en su registro.
	if code := h.reenviar(t, "otro.example.com"); code != http.StatusForbidden {
		t.Fatalf("forward to another domain: %d", code)
	}
	// En none, ni siquiera el de la credencial.
	h.api(t, "PUT", "/kling/network", `{"egress":"none"}`)
	if code := h.reenviar(t, "api.example.com"); code != http.StatusForbidden {
		t.Fatalf("forward in none: %d", code)
	}

	// Otro programa del mismo usuario (curl) no llega al API.
	out2, err := exec.Command("/usr/bin/curl", "-s", "-m", "3", "--unix-socket", filepath.Join(h.dir, credproxy.SplitAPISock), "http://x/kling/info").CombinedOutput()
	if err == nil || strings.Contains(string(out2), "credproxy") {
		t.Fatalf("curl reached the API: %v %s", err, out2)
	}

	// Muere su VMM: sale él también.
	h.vmm.Process.Kill()
	select {
	case <-h.fin:
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy outlived its VMM")
	}
	b, err := os.ReadFile(filepath.Join(h.dir, credproxy.AuditFile))
	if err != nil || !strings.Contains(string(b), `"reason":"no_credential"`) || !strings.Contains(string(b), `"reason":"disabled"`) || strings.Contains(string(b), "sk-1-clave") {
		t.Fatalf("audit log: %q %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(h.dir, credproxy.AuditFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("audit log mode: %v %v", fi, err)
	}
	for _, s := range []string{credproxy.SplitAPISock, credproxy.SplitFwdSock} {
		if _, err := os.Stat(filepath.Join(h.dir, s)); err == nil {
			t.Errorf("%s is still there", s)
		}
	}
}

// Un directorio que otros pueden tocar, o un VMM que ya no existe: no arranca.
func TestNoArrancaSinLoSuyo(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "kcp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := dirPropio(base); err == nil {
		t.Fatal("dirPropio accepted a 0755 directory")
	}
	if _, err := vigilarVMM(1 << 30); err == nil {
		t.Fatal("vigilarVMM accepted a pid that does not exist")
	}
}
