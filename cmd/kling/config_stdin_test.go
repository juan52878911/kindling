package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/config"
)

// `kling config set gateway.token -` lee el valor de stdin: como argumento,
// un secreto queda en ps y en el historial del shell.
func TestConfigSetDesdeStdin(t *testing.T) {
	t.Setenv("KLING_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	const tok = "tok-9f8e7d6c5b4a39281706"

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = prev })
	if _, err := w.WriteString(tok + "\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()

	out, err := salida(t, func() error { return cmdConfig([]string{"set", "gateway.token", "-"}) })
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gateway.Token != tok {
		t.Fatalf("gateway.token = %q, want the value from stdin", cfg.Gateway.Token)
	}
	if strings.Contains(out, tok) {
		t.Errorf("config set printed the token in full: %q", out)
	}

	// -reveal lo da entero, para la tubería; sin él, enmascarado.
	out, err = salida(t, func() error { return cmdConfig([]string{"get", "gateway.token", "-reveal"}) })
	if err != nil || strings.TrimSpace(out) != tok {
		t.Errorf("get -reveal = %q, %v", out, err)
	}
	out, err = salida(t, func() error { return cmdConfig([]string{"get", "gateway.token"}) })
	if err != nil || strings.Contains(out, tok) {
		t.Errorf("get without -reveal = %q, %v", out, err)
	}
}
