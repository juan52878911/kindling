package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvQuote(t *testing.T) {
	for in, want := range map[string]string{
		"abc-123_.": "abc-123_.",
		"":          "''",
		"a b":       "'a b'",
		"a'b":       `'a'\''b'`,
		"$(x)`y`\n": "'$(x)`y`\n'",
	} {
		if got := envQuote(in); got != want {
			t.Errorf("envQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHookRechazaHooksPathFuera(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	// Dentro del árbol de trabajo (versionado).
	gitT(t, dir, "config", "core.hooksPath", ".husky")
	if err := ta.branchHook(ctx, "install"); err == nil || !strings.Contains(err.Error(), "-force") {
		t.Fatalf("hooksPath in the working tree: err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".husky", hookName)); err == nil {
		t.Error("the hook was written anyway")
	}
	// El propio directorio git: cuelga de él (rel == ".") pero no es un
	// directorio de hooks.
	gitT(t, dir, "config", "core.hooksPath", ".git")
	if err := ta.branchHook(ctx, "install"); err == nil || !strings.Contains(err.Error(), "git directory itself") {
		t.Fatalf("hooksPath = .git: err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", hookName)); err == nil {
		t.Error("the hook was written into the git directory anyway")
	}
	// Un subdirectorio del directorio git sí vale (el de siempre).
	gitT(t, dir, "config", "core.hooksPath", filepath.Join(".git", "hooks-kling"))
	if err := ta.branchHook(ctx, "install"); err != nil {
		t.Fatalf("hooksPath = .git/hooks-kling: %v", err)
	}
	if err := ta.branchHook(ctx, "uninstall"); err != nil {
		t.Fatal(err)
	}
	// Fuera del repo.
	out := filepath.Join(t.TempDir(), "global-hooks")
	gitT(t, dir, "config", "core.hooksPath", out)
	if err := ta.branchHook(ctx, "install"); err == nil {
		t.Fatal("hooksPath outside the repo was accepted")
	}
	// Con -force sí.
	ta.hookForce = true
	if err := ta.branchHook(ctx, "install"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, hookName)); err != nil {
		t.Error(err)
	}
}
