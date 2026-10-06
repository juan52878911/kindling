package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/ext/mcp/internal/mcp"
)

// fakeImageScript escribe un 80-mcp-image.sh falso y apunta a él el builder.
func fakeImageScript(t *testing.T, body string) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "80-mcp-image.sh")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\nset -eu\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KLING_IMAGE_SCRIPT", script)
	t.Setenv("KLING_BRIDGE", "")
}

func writeRequest(t *testing.T, dir string, req mcp.BuildRequest) {
	t.Helper()
	b, err := json.Marshal(req.API())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "request.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Las variables de `kling add -env` no pueden viajar en el argv del script (un
// proceso root: ps las enseña a cualquiera del host). Llegan en ENV_FILE, un
// fichero 0600 que se borra al acabar. El script falso deja constancia de su
// argv y de lo que encontró en ENV_FILE.
func TestBuilderEnvPorFichero(t *testing.T) {
	work := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	fakeImageScript(t, `{
  printf 'ARGV %s\n' "$*"
  printf 'MODE %s\n' "$(stat -c %a "$ENV_FILE" 2>/dev/null || stat -f %Lp "$ENV_FILE")"
  cat "$ENV_FILE"
} > '`+out+"'\n")
	writeRequest(t, work, mcp.BuildRequest{Name: "svc", Env: []string{"A=secreto", "B=x y"}, Cmd: []string{"srv"}})

	if err := cmdBuilder([]string{mcp.Builder, work}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	argv := strings.SplitN(s, "\n", 2)[0]
	if strings.Contains(argv, "secreto") || strings.Contains(argv, " -e ") {
		t.Errorf("la variable viajó en el argv: %q", argv)
	}
	if !strings.Contains(s, "MODE 600\n") {
		t.Errorf("ENV_FILE no es 0600:\n%s", s)
	}
	if !strings.HasSuffix(s, "A=secreto\nB=x y\n") {
		t.Errorf("contenido de ENV_FILE inesperado:\n%s", s)
	}
	ents, _ := os.ReadDir(work)
	for _, e := range ents {
		if e.Name() != "request.json" {
			t.Errorf("ENV_FILE no se borró al acabar: %s", e.Name())
		}
	}
}

// Sin variables no se pasa ENV_FILE.
func TestBuilderSinEnv(t *testing.T) {
	work := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	fakeImageScript(t, `printf '%s' "${ENV_FILE-unset}" > '`+out+"'\n")
	t.Setenv("ENV_FILE", "")
	os.Unsetenv("ENV_FILE")
	writeRequest(t, work, mcp.BuildRequest{Name: "svc", Cmd: []string{"srv"}})

	if err := cmdBuilder([]string{mcp.Builder, work}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); string(got) != "unset" {
		t.Errorf("sin -env no debería pasarse ENV_FILE: %q", got)
	}
}
