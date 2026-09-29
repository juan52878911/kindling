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
