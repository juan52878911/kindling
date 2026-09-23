package main

import (
	"flag"
	"reflect"
	"strings"
	"testing"
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

// Los comandos MCP siguen en la ayuda y en el completado aunque ya no estén en
// el switch del núcleo: llegan por la extensión incorporada.
func TestAyudaYCompletadoIncluyenLaExtensionMCP(t *testing.T) {
	var b strings.Builder
	printUsage(&b)
	for _, want := range []string{"MCP SERVICES", "mcp import <service>", "CONNECT YOUR AGENT", "GATEWAY", "EXTENSIONS"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("la ayuda perdió %q", want)
		}
	}
	for _, sh := range []bool{false, true} {
		script := completionScript(sh)
		for _, want := range []string{" mcp ", "import list", "status enable disable"} {
			if !strings.Contains(script, want) {
				t.Errorf("completado (zsh=%v) sin %q", sh, want)
			}
		}
	}
	// Ningún comando MCP se cuela en la lista del núcleo.
	for _, c := range coreCommands {
		switch c {
		case "mcp", "add", "search", "connect", "migrate", "memory", "gateway", "export":
			t.Errorf("%q es de la extensión MCP, no del núcleo", c)
		}
	}
	if p := extensions().Lookup("mcp"); p == nil || p.Builtin == nil {
		t.Fatal("`kling mcp` tiene que servirlo la extensión incorporada")
	}
}
