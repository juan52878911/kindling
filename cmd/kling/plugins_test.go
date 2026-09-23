package main

import (
	"flag"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/plugin"
)

// knownFlags separa lo que el núcleo entiende de lo que es de una extensión:
// un flag desconocido no puede romper `kling status`.
func TestKnownFlags(t *testing.T) {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.String("H", "", "")
	fs.Bool("json", false, "")
	got := knownFlags(fs, []string{"-gateway", "http://x", "-H", "ssh://lab", "-json", "-otro=1", "-H=unix:///s"})
	want := []string{"-H", "ssh://lab", "-json", "-H=unix:///s"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("knownFlags = %q, quería %q", got, want)
	}
	if err := fs.Parse(got); err != nil {
		t.Fatalf("lo filtrado tiene que parsear: %v", err)
	}
}

// El núcleo no trae MCP: sin extensiones, la ayuda y el completado solo tienen
// lo suyo. Con una extensión instalada, lo que ella declara aparece en los dos.
func TestAyudaYCompletadoConUnaExtension(t *testing.T) {
	reset := func() { extOnce = sync.Once{}; extReg = nil }
	t.Cleanup(reset)
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("sin go en el PATH no se puede compilar la extensión de prueba")
	}

	t.Setenv("KLING_PLUGIN_PATH", t.TempDir())
	t.Setenv("PATH", "/usr/bin:/bin") // que no encuentre extensiones reales del usuario
	reset()
	var b strings.Builder
	printUsage(&b)
	for _, no := range []string{"MCP SERVICES", "mcp import", "CONNECT YOUR AGENT"} {
		if strings.Contains(b.String(), no) {
			t.Errorf("la ayuda del núcleo no debe traer %q", no)
		}
	}
	for _, c := range coreCommands {
		switch c {
		case "mcp", "add", "search", "connect", "migrate", "memory", "gateway", "export":
			t.Errorf("%q es de kindling-mcp, no del núcleo", c)
		}
	}

	dir := t.TempDir()
	out, err := exec.Command(goBin, "build", "-o", filepath.Join(dir, "kling-hello"), "../../pkg/plugin/testdata/kling-hello").CombinedOutput()
	if err != nil {
		t.Fatalf("compilando la extensión de prueba: %v\n%s", err, out)
	}
	t.Setenv("KLING_PLUGIN_PATH", dir)
	// El plazo del manifiesto no es lo que se prueba aquí, y con 2 s el test
	// fallaba de vez en cuando bajo `go test -race ./...` con la CPU ocupada
	// por el resto de paquetes: la extensión tardaba más en ARRANCAR que eso.
	previo := plugin.ManifestTimeout
	plugin.ManifestTimeout = 30 * time.Second
	t.Cleanup(func() { plugin.ManifestTimeout = previo })
	reset()
	b.Reset()
	printUsage(&b)
	if !strings.Contains(b.String(), "HELLO") || !strings.Contains(b.String(), "says hello") {
		t.Errorf("la ayuda no incluye la extensión instalada:\n%s", b.String())
	}
	for _, zsh := range []bool{false, true} {
		if s := completionScript(zsh); !strings.Contains(s, " hello") || !strings.Contains(s, "world") {
			t.Errorf("el completado (zsh=%v) no incluye la extensión", zsh)
		}
	}
	if p := extensions().Lookup("hello"); p == nil || p.Path == "" {
		t.Fatal("`kling hello` tiene que servirlo la extensión externa")
	}
	if extensions().Lookup("ps") != nil {
		t.Fatal("un comando del núcleo nunca se cede a una extensión")
	}
}
