package main

import (
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/config"
	"github.com/juan52878911/kindling/pkg/plugin"
)

// salida ejecuta f con os.Stdout redirigido y devuelve lo que imprimió.
func salida(t *testing.T, f func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	ferr := f()
	w.Close()
	os.Stdout = prev
	return <-done, ferr
}

// El ciclo entero desde la CLI: instalar desde un fichero local con su
// compañero, verlo en `plugins ls -json` con su hash, desactivarlo, volver a
// activarlo y quitarlo.
func TestPluginsInstallLsDisableRm(t *testing.T) {
	reset := func() { extOnce = sync.Once{}; extReg = nil }
	t.Cleanup(reset)
	previo := plugin.ManifestTimeout
	plugin.ManifestTimeout = 30 * time.Second
	t.Cleanup(func() { plugin.ManifestTimeout = previo })

	dir := t.TempDir()
	hermetic(t)
	t.Setenv("KLING_PLUGIN_PATH", dir)
	t.Setenv("KLING_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("PATH", "/usr/bin:/bin")

	src := t.TempDir()
	suffix := "-" + runtime.GOOS + "-" + runtime.GOARCH
	manifest := `{"manifest_version":1,"name":"demo","version":"2.0.0","commands":[{"name":"demo"},{"name":"ps","top_level":true}],"companions":["kling-demo-helper"]}`
	os.WriteFile(filepath.Join(src, "kling-demo"+suffix),
		[]byte("#!/bin/sh\nif [ \"$1\" = --kling-manifest ]; then echo '"+manifest+"'; fi\n"), 0o644)
	os.WriteFile(filepath.Join(src, "kling-demo-helper"+suffix), []byte("#!/bin/sh\n"), 0o644)

	reset()
	out, err := salida(t, func() error {
		return cmdPlugins([]string{"install", "demo", "-file", filepath.Join(src, "kling-demo"+suffix)})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"demo 2.0.0 installed", "kling demo", "ignored ps", "kling-demo-helper", "completion"} {
		if !strings.Contains(out, want) {
			t.Errorf("la salida de install no dice %q:\n%s", want, out)
		}
	}

	ls := func() []pluginRow {
		t.Helper()
		reset()
		out, err := salida(t, func() error { return cmdPlugins([]string{"ls", "-json"}) })
		if err != nil {
			t.Fatal(err)
		}
		var all, rows []pluginRow
		if err := json.Unmarshal([]byte(out), &all); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		// Las incorporadas (ai, chispa, models) salen siempre; aquí solo
		// interesan las instaladas.
		for _, r := range all {
			if !r.Builtin {
				rows = append(rows, r)
			}
		}
		return rows
	}
	rows := ls()
	if len(rows) != 1 || rows[0].Name != "demo" || rows[0].Status != "ok" || len(rows[0].SHA256) != 64 ||
		!strings.HasPrefix(rows[0].URL, "file://") || rows[0].Modified {
		t.Fatalf("plugins ls: %+v", rows)
	}

	// Si alguien cambia el binario, ls lo dice.
	os.WriteFile(filepath.Join(dir, "kling-demo"), []byte("#!/bin/sh\nif [ \"$1\" = --kling-manifest ]; then echo '"+manifest+"'; fi\n# otro\n"), 0o755)
	if rows = ls(); !rows[0].Modified {
		t.Fatal("un binario cambiado tras instalar tiene que marcarse")
	}

	if _, err := salida(t, func() error { return cmdPlugins([]string{"disable", "demo"}) }); err != nil {
		t.Fatal(err)
	}
	c, _ := config.Load()
	if !c.PluginDisabled("demo") {
		t.Fatal("disable no quedó en config.json")
	}
	if rows = ls(); rows[0].Status != "disabled" || !rows[0].Disabled {
		t.Fatalf("desactivada: %+v", rows[0])
	}
	if extensions().Lookup("demo") != nil || extensions().DisabledFor("demo") == nil {
		t.Fatal("una desactivada no recibe su comando")
	}
	if _, err := salida(t, func() error { return cmdPlugins([]string{"enable", "demo"}) }); err != nil {
		t.Fatal(err)
	}
	if rows = ls(); rows[0].Status != "ok" {
		t.Fatalf("reactivada: %+v", rows[0])
	}

	reset()
	if _, err := salida(t, func() error { return cmdPlugins([]string{"rm", "demo"}) }); err != nil {
		t.Fatal(err)
	}
	if es, _ := os.ReadDir(dir); len(es) != 0 {
		t.Fatalf("rm dejó %d ficheros", len(es))
	}
}

func TestPluginsInstallEnDesarrolloPideEtiqueta(t *testing.T) {
	if Version != "dev" {
		t.Skip("solo en un binario de desarrollo")
	}
	t.Setenv("KLING_PLUGIN_PATH", t.TempDir())
	err := cmdPlugins([]string{"install", "mcp"})
	if err == nil || !strings.Contains(err.Error(), "@v") {
		t.Fatalf("err = %v", err)
	}
	if err := cmdPlugins([]string{"install", "mcp@v1.0.0", "-file", "x"}); err == nil {
		t.Fatal("@tag y -file no van juntos")
	}
}

func TestParseInterspersed(t *testing.T) {
	fs := newTestFlagSet()
	pos, err := parseInterspersed(fs, []string{"mcp@v1", "-from", "https://x/y", "--", "-raro"})
	if err != nil || strings.Join(pos, ",") != "mcp@v1,-raro" || fs.Lookup("from").Value.String() != "https://x/y" {
		t.Fatalf("%v %v", pos, err)
	}
}

func newTestFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("from", "", "")
	return fs
}
