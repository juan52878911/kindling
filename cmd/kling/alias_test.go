package main

import (
	"flag"
	"io"
	"strings"
	"testing"
)

// Los alias que avisaban desde v0.18 se retiraron en 1.0: dan un error de uso
// (2) con el nombre nuevo. Los permanentes y los nombres de ahora, no.
func TestAliasRetirados(t *testing.T) {
	hermetic(t)
	for in, want := range map[string]string{
		"add":       "kling add was removed in 1.0: use kling mcp add",
		"rmi":       "kling rmi was removed in 1.0: use kling template rm",
		"info":      "kling info was removed in 1.0: use kling status -v",
		"models":    "kling models was removed in 1.0: use kling ai model",
		"sandboxes": "kling sandboxes was removed in 1.0: use kling sandbox",
		"gateway":   "kling gateway was removed in 1.0: use kling mcp serve",
		"commit":    "",
		"snapshots": "",
		"plugins":   "",
		"ps":        "",
		"template":  "",
	} {
		err := removedAliasError(in)
		got := ""
		if err != nil {
			got = err.Error()
			if codigoDeSalida(err) != salidaUso {
				t.Errorf("%s: código %d, quería %d", in, codigoDeSalida(err), salidaUso)
			}
			if h := hintFor(err); !strings.HasPrefix(h, "kling help ") {
				t.Errorf("%s: pista %q", in, h)
			}
		}
		if got != want {
			t.Errorf("removedAliasError(%q) = %q, want %q", in, got, want)
		}
	}
	// Los retirados siguen reservados al núcleo; los de kling-mcp, no.
	for _, a := range []string{"rmi", "info", "models", "chispa"} {
		if !contains(coreCommands, a) {
			t.Errorf("%q tiene que seguir reservado al núcleo", a)
		}
	}
}

// -cpu y -vcpus se retiraron en 1.0: el error dice el nombre nuevo.
func TestFlagRetirado(t *testing.T) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Int("cpu-pct", 0, "")
	flagRetirado(fs, "cpu", "cpu-pct")
	err := fs.Parse([]string{"-cpu", "50"})
	if err == nil || !strings.Contains(err.Error(), "-cpu was removed in 1.0: use -cpu-pct") {
		t.Fatalf("%v", err)
	}
}
