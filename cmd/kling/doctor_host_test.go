package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestDoctorDeRuntime(t *testing.T) {
	cs := doctorDeRuntime([]check{
		{label: "firecracker", ok: true, found: "/usr/bin/firecracker"},
		{label: "nft (nftables)", ok: false, found: "missing", why: "no IPv6 isolation", fix: []string{"apt install nftables"}},
	})
	if len(cs) != 2 || cs[0].Name != "runtime firecracker" || cs[0].State != doctorOK || cs[0].Fix != "" {
		t.Fatalf("%+v", cs)
	}
	if cs[1].State != doctorFail || cs[1].Detail != "missing: no IPv6 isolation" || cs[1].Fix != "apt install nftables" {
		t.Fatalf("%+v", cs[1])
	}
}

func TestCheckSocket(t *testing.T) {
	if _, ok := checkSocket(sondearSocket("ssh://u@h")); ok {
		t.Fatal("un ssh:// no tiene socket local")
	}
	dir, err := os.MkdirTemp("/tmp", "kt")
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	c, _ := checkSocket(sondearSocket("unix://" + dir + "/no.sock"))
	if c.State != doctorFail || !strings.Contains(c.Detail, "no socket") || !strings.HasPrefix(c.Fix, "kling up") {
		t.Fatalf("sin socket: %+v", c)
	}
	fichero := filepath.Join(dir, "f")
	os.WriteFile(fichero, nil, 0o600)
	if c, _ := checkSocket(sondearSocket(fichero)); c.State != doctorFail || !strings.Contains(c.Detail, "not a socket") {
		t.Fatalf("fichero: %+v", c)
	}
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	if c, _ := checkSocket(sondearSocket(sock)); c.State != doctorOK {
		t.Fatalf("socket: %+v", c)
	}
	if os.Geteuid() != 0 {
		os.Chmod(sock, 0o400)
		if c, _ := checkSocket(sondearSocket(sock)); c.State != doctorFail || !strings.Contains(c.Fix, "KLING_SOCKET_USER") {
			t.Fatalf("sin permiso: %+v", c)
		}
	}
}

func TestCheckDisco(t *testing.T) {
	if _, ok := checkDisco(&api.Info{}); ok {
		t.Fatal("un daemon sin disk_free_bytes no opina")
	}
	gib := int64(1 << 30)
	for _, c := range []struct {
		info  api.Info
		state string
	}{
		{api.Info{DiskFreeBytes: 50 * gib, Backend: "firecracker"}, doctorOK},
		{api.Info{DiskFreeBytes: 3 * gib, Backend: "firecracker"}, doctorWarn},
		{api.Info{DiskFreeBytes: gib, Backend: "firecracker"}, doctorFail},
		// macOS: 16 GiB de mínimo.
		{api.Info{DiskFreeBytes: 20 * gib, Backend: "hv"}, doctorWarn},
		// El ajuste del daemon manda.
		{api.Info{DiskFreeBytes: gib, Tuning: map[string]string{"KLING_MIN_FREE_DISK_MIB": "512"}}, doctorOK},
	} {
		got, _ := checkDisco(&c.info)
		if got.State != c.state {
			t.Errorf("%+v: %+v", c.info, got)
		}
	}
}

// doctor -json da las comprobaciones del runtime como checks[] (una por
// entrada), no el texto de `up -check` en runtime_report; y con el socket.
func TestDoctorJSONEstructurado(t *testing.T) {
	hermetic(t)
	t.Setenv("KLING_HOST", "unix:///nonexistent/kling.sock")
	t.Setenv("KLING_ROOT", t.TempDir())
	out, err := capturarStdout(t, func() error { return cmdDoctor([]string{"-json"}) })
	if err == nil {
		t.Fatal("sin daemon doctor sale con 1")
	}
	var r map[string]any
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, ok := r["runtime_report"]; ok {
		t.Fatal("sigue el texto de up -check")
	}
	var nombres []string
	runtime := 0
	for _, c := range r["checks"].([]any) {
		n := c.(map[string]any)["name"].(string)
		nombres = append(nombres, n)
		if strings.HasPrefix(n, "runtime ") {
			runtime++
		}
	}
	if runtime == 0 || !strings.Contains(strings.Join(nombres, ","), "socket") {
		t.Fatalf("checks: %v", nombres)
	}
}
