package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/plugin"
)

func findCheck(cs []doctorCheck, name string) *doctorCheck {
	for i := range cs {
		if cs[i].Name == name {
			return &cs[i]
		}
	}
	return nil
}

func TestDoctorTodoBien(t *testing.T) {
	in := doctorInput{
		endpoint: "/run/kling.sock", cliVersion: "v0.13.0",
		info:       &api.Info{Version: "0.13.0", Machines: 2},
		plugins:    []*plugin.Plugin{{Name: "mcp", Path: "/x/kling-mcp", Manifest: &plugin.Manifest{Version: "0.13.0"}}},
		completion: true, extDir: "/h/.local/share/kling/plugins", extDirOK: true,
		pathDirs: []string{"/usr/bin", "/h/.local/share/kling/plugins/"},
	}
	cs := doctorChecks(in)
	for _, c := range cs {
		if c.State != doctorOK {
			t.Errorf("%s: %s %s", c.Name, c.State, c.Detail)
		}
	}
	var b bytes.Buffer
	writeDoctor(&b, cs)
	if !strings.Contains(b.String(), "everything looks good") {
		t.Fatal(b.String())
	}
}

func TestDoctorProblemas(t *testing.T) {
	in := doctorInput{
		endpoint: "/run/kling.sock", cliVersion: "v0.13.0",
		info:    &api.Info{}, // Info devuelve un puntero aunque falle
		infoErr: errors.New("cannot talk to the daemon at /run/kling.sock: dial unix: connect: permission denied"),
		plugins: []*plugin.Plugin{
			{Name: "old", Path: "/x/kling-old", Err: errors.New("needs kling 9.0.0 or newer (this is 0.13.0)")},
			{Name: "off", Path: "/x/kling-off", Err: errors.New("disabled (kling plugins enable off)")},
		},
		extDir: "/h/p", extDirOK: true,
	}
	cs := doctorChecks(in)
	if d := findCheck(cs, "daemon"); d == nil || d.State != doctorFail || !strings.Contains(d.Fix, "KLING_SOCKET_USER") {
		t.Fatalf("daemon: %+v", d)
	}
	if findCheck(cs, "versions") != nil {
		t.Fatal("sin daemon no se comparan versiones")
	}
	if c := findCheck(cs, "extension old"); c.State != doctorFail || !strings.Contains(c.Fix, "upgrade kling") {
		t.Fatalf("%+v", c)
	}
	if c := findCheck(cs, "extension off"); c.State != doctorOK {
		t.Fatalf("desactivada es una decisión, no una avería: %+v", c)
	}
	if c := findCheck(cs, "completion"); c.State != doctorWarn || c.Fix != "kling completion install" {
		t.Fatalf("%+v", c)
	}
	if c := findCheck(cs, "extensions dir"); c.State != doctorWarn || !strings.Contains(c.Fix, `export PATH="/h/p:$PATH"`) {
		t.Fatalf("%+v", c)
	}
	var b bytes.Buffer
	writeDoctor(&b, cs)
	if !strings.Contains(b.String(), "✗ daemon") || !strings.Contains(b.String(), "fix: kling completion install") ||
		!strings.Contains(b.String(), "2 problem(s)") {
		t.Fatal(b.String())
	}
}

func TestDoctorVersionesDistintas(t *testing.T) {
	cs := doctorChecks(doctorInput{cliVersion: "v0.13.0", info: &api.Info{Version: "0.12.0"}, completion: true})
	if v := findCheck(cs, "versions"); v == nil || v.State != doctorWarn {
		t.Fatalf("%+v", v)
	}
	cs = doctorChecks(doctorInput{cliVersion: "dev", info: &api.Info{Version: "0.12.0"}, completion: true})
	if v := findCheck(cs, "versions"); v.State != doctorOK {
		t.Fatalf("un kling de desarrollo no se compara: %+v", v)
	}
}

func TestExtensionsDir(t *testing.T) {
	t.Setenv("KLING_PLUGIN_PATH", "/a:/b")
	if extensionsDir() != "/a" {
		t.Fatal(extensionsDir())
	}
	t.Setenv("KLING_PLUGIN_PATH", "")
	t.Setenv("XDG_DATA_HOME", "/xdg")
	if extensionsDir() != "/xdg/kling/plugins" {
		t.Fatal(extensionsDir())
	}
}

func TestCaptureStdout(t *testing.T) {
	out, err := captureStdout(func() error {
		fmt.Print("hola\n")
		return errors.New("x")
	})
	if out != "hola\n" || err == nil {
		t.Fatalf("%q %v", out, err)
	}
}
