package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFishCompletion(t *testing.T) {
	t.Setenv("KLING_PLUGIN_PATH", t.TempDir())
	s := fishCompletion()
	for _, want := range []string{
		"set -gx _KLING_COMPLETION 1",
		"complete -c kling -f",
		"complete -c kling -n __fish_use_subcommand -a \"",
		"__fish_seen_subcommand_from snapshots' -a \"ls rm inspect\"",
		"__fish_seen_subcommand_from volume volumes' -a \"create ls rm populate\"",
		"__fish_seen_subcommand_from completion' -a \"bash zsh fish install\"",
		"(kling ps -q 2>/dev/null)",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("falta %q en:\n%s", want, s)
		}
	}
	for _, c := range []string{" doctor", " try"} {
		if !strings.Contains(s, c) {
			t.Errorf("el completado no ofrece%s", c)
		}
	}
	if strings.Contains(s, "dial-stdio") {
		t.Error("dial-stdio no es para teclearlo")
	}
}

func TestCompletionExportaLaMarca(t *testing.T) {
	t.Setenv("KLING_PLUGIN_PATH", t.TempDir())
	for _, sh := range completionShells {
		if !strings.Contains(completionFor(sh), "_KLING_COMPLETION") {
			t.Errorf("%s no exporta _KLING_COMPLETION: doctor no sabría si está cargado", sh)
		}
	}
	// help completa con los comandos.
	if !strings.Contains(completionScript(false), "help) COMPREPLY=( $(compgen -W \"up ") {
		t.Error("help no completa con los comandos")
	}
}

func TestCompletionInstall(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KLING_PLUGIN_PATH", t.TempDir())
	t.Setenv("KLING_CONFIG", filepath.Join(dir, "kling", "config.json"))
	for _, sh := range completionShells {
		if err := completionInstall(io.Discard, sh); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dir, "kling", "completion."+sh))
		if err != nil || !strings.Contains(string(b), "kling") {
			t.Fatalf("%s: %v", sh, err)
		}
		path, line := completionInstallPath(sh)
		if !strings.Contains(line, path) {
			t.Fatalf("la línea del rc ha de cargar %s: %s", path, line)
		}
	}
	t.Setenv("SHELL", "/bin/tcsh")
	if err := completionInstall(io.Discard, ""); err == nil || !strings.Contains(hintFor(err), "bash|zsh|fish") {
		t.Fatalf("una shell desconocida: %v", err)
	}
}
