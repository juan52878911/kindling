package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/config"
)

// Con token, `connect` no llama a `claude mcp add --header ...`: el token
// quedaría en el argv del proceso, que cualquiera lee en ps. Parchea el
// fichero de Claude Code (su ámbito de usuario) en su lugar.
func TestInstallConTokenNoLoPasaPorArgv(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	argv := filepath.Join(tmp, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + argv + "\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/bin:/usr/bin")

	c, _ := findClient("claude-code")
	if err := c.install("kindling", testURL, testToken); err != nil {
		t.Fatalf("install: %v", err)
	}
	if b, err := os.ReadFile(argv); err == nil && strings.Contains(string(b), testToken) {
		t.Fatalf("the token went through claude's argv:\n%s", b)
	}
	b, err := os.ReadFile(filepath.Join(tmp, ".claude.json"))
	if err != nil || !strings.Contains(string(b), testToken) {
		t.Fatalf("~/.claude.json was not patched with the token: %v %s", err, b)
	}
	// El fragmento manual tampoco sugiere la orden con el token dentro.
	if s := c.text("kindling", testURL, testToken); strings.Contains(s, "--header") {
		t.Errorf("the manual snippet suggests a command with the token in argv:\n%s", s)
	}
	z, _ := findClient("zed")
	if s := z.text("kindling", testURL, testToken); strings.Contains(s, "Bearer "+testToken+"\"]") ||
		strings.Contains(s, `"--header","Authorization: Bearer`) {
		t.Errorf("the zed snippet puts the token in mcp-remote's args:\n%s", s)
	}
}

// El gateway que genera su token no lo imprime entero (bajo systemd la salida
// va al journal) ni sugiere pasarlo como argumento.
func TestGatewayNoImprimeElToken(t *testing.T) {
	t.Setenv("KLING_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("KLING_GATEWAY_TOKEN", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	tok, err := resolveGatewayToken(cfg, false, "127.0.0.1:0")
	w.Close()
	os.Stdout = prev
	out := <-done
	if err != nil || tok == "" {
		t.Fatalf("resolveGatewayToken = %q, %v", tok, err)
	}
	if strings.Contains(out, tok) {
		t.Errorf("the token was printed in full:\n%s", out)
	}
	// La pista buena acaba en "gateway.token -" (leer de stdin): se quita antes
	// de buscar, o un token que empiece por '-' daría un falso positivo.
	if strings.Contains(strings.ReplaceAll(out, tokenPipeHint, ""), "config set gateway.token ") {
		t.Errorf("it suggests the token as an argument:\n%s", out)
	}
	saved, err := config.Load()
	if err != nil || saved.Gateway.Token != tok {
		t.Fatalf("token not saved: %v", err)
	}
}
