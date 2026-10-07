package main

import (
	"encoding/json"
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
