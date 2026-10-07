package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/internal/upgrade"
)

// `upgrade -schemas` es lo que otro kling le pregunta a este antes de
// instalarlo: su versión y lo que sabe leer, sin hablar con ningún daemon.
func TestUpgradeSchemas(t *testing.T) {
	t.Setenv("KLING_HOST", "unix:///nonexistent/kling.sock")
	out, err := salida(t, func() error { return cmdUpgrade([]string{"-schemas"}) })
	if err != nil {
		t.Fatal(err)
	}
	var ib upgrade.InfoBinario
	if err := json.Unmarshal([]byte(out), &ib); err != nil {
		t.Fatalf("%v: %q", err, out)
	}
	if ib.Kling != Version || ib.Esquemas != machine.EsquemasSoportados() || ib.API == 0 {
		t.Errorf("%+v", ib)
	}
}

func TestUpgradeRechazaArgumentos(t *testing.T) {
	for _, args := range [][]string{{"v1.2.3"}, {"-rollback", "-tag", "v1.2.3"}} {
		if _, err := salida(t, func() error { return cmdUpgrade(args) }); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestProgramaLaunchd(t *testing.T) {
	out := []byte("gui/501/dev.kindling.daemon = {\n\tactive count = 1\n\tpath = /Users/j/Library/LaunchAgents/dev.kindling.daemon.plist\n\tprogram = /Users/j/.local/bin/kling\n\targuments = {\n\t\t/Users/j/.local/bin/kling\n\t\tdaemon\n\t}\n")
	if got := programaLaunchd(out); got != "/Users/j/.local/bin/kling" {
		t.Errorf("got %q", got)
	}
	if got := programaLaunchd([]byte("nothing")); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestArgsRemotos(t *testing.T) {
	if got := argsRemotos("v0.18.0", false); got != " -tag v0.18.0" {
		t.Errorf("%q", got)
	}
	if got := argsRemotos("", true); !strings.Contains(got, "-rollback") {
		t.Errorf("%q", got)
	}
}

// El daemon del socket tiene que ser el del servicio: con KLING_HOST en un
// daemon privado, upgrade no puede reiniciar el de producción.
func TestMismoProceso(t *testing.T) {
	if err := mismoProceso("/run/kling.sock", "kling.service", 42, 42); err != nil {
		t.Error(err)
	}
	if err := mismoProceso("/run/kt/kling.sock", "kling.service", 42, 77); err == nil || !strings.Contains(err.Error(), "pid 77") {
		t.Errorf("another daemon accepted: %v", err)
	}
	if err := mismoProceso("/run/kt/kling.sock", "kling.service", 0, 77); err == nil {
		t.Error("a unit that is not running accepted")
	}
}

// pidDelPar lo dice el kernel: un socket que escucha este proceso da su PID.
func TestPidDelPar(t *testing.T) {
	dir, err := os.MkdirTemp("", "kp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			c.Read(make([]byte, 1))
		}
	}()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	pid, err := pidDelPar(c.(*net.UnixConn))
	if err != nil || pid != os.Getpid() {
		t.Fatalf("pid %d, %v; want %d", pid, err, os.Getpid())
	}
}

func TestPidLaunchd(t *testing.T) {
	if got := pidLaunchd([]byte("gui/501/dev.kindling.daemon = {\n\tstate = running\n\tpid = 4242\n")); got != 4242 {
		t.Errorf("got %d", got)
	}
	if got := pidLaunchd([]byte("state = not running\n")); got != 0 {
		t.Errorf("got %d", got)
	}
}
