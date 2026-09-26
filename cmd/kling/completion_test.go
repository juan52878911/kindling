package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFishCompletion(t *testing.T) {
	hermetic(t)
	s := fishCompletion()
	for _, want := range []string{
		"set -gx _KLING_COMPLETION 1",
		"complete -c kling -f",
		"complete -c kling -n __fish_use_subcommand -a \"",
		"__fish_seen_subcommand_from template' -a \"ls inspect rm\"",
		"__fish_seen_subcommand_from volume' -a \"create ls rm populate\"",
		"__fish_seen_subcommand_from ai' -a \"up serve ls test generate eval calibrate reload prime review feedback retrain rollback model chispa\"",
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
	hermetic(t)
	for _, sh := range completionShells {
		if !strings.Contains(completionFor(sh), "_KLING_COMPLETION") {
			t.Errorf("%s no exporta _KLING_COMPLETION: doctor no sabría si está cargado", sh)
		}
	}
	// help completa con los comandos.
	if !strings.Contains(completionScript(false), "help) COMPREPLY=( $(compgen -W \"all up ") {
		t.Error("help no completa con los comandos")
	}
	// Los alias y lo avanzado no se completan; los sustantivos nuevos sí.
	first := completionCommands()
	for _, no := range []string{"snapshots", "rmi", "commit", "images", "plugins", "models", "chispa", "info", "mmds", "squeeze", "daemon", "dial-stdio", "builder"} {
		if contains(strings.Fields(first), no) {
			t.Errorf("%q no debe completarse en primer nivel", no)
		}
	}
	for _, yes := range []string{"save", "template", "image", "plugin", "ai", "machine"} {
		if yes == "machine" {
			continue // avanzado
		}
		if !contains(strings.Fields(first), yes) {
			t.Errorf("%q falta en el primer nivel", yes)
		}
	}
	// Quien recibe una máquina la completa con ps -q.
	if ma := machineArgs(); !strings.Contains(ma, "freeze") || !strings.Contains(ma, "save") || strings.Contains(ma, "template") {
		t.Errorf("machineArgs = %q", ma)
	}
}

func TestCompletionInstall(t *testing.T) {
	hermetic(t)
	dir := t.TempDir()
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
