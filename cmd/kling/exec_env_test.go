package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// -e KEY toma el valor del entorno del CLI y -env-file de un fichero: así un
// secreto no tiene que ir en el argv de kling (ps, historial).
func TestResolveEnv(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "env")
	if err := os.WriteFile(f, []byte("# comentario\n\nexport DB_URL=postgres://x\nTOKEN=a=b\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lookup := func(k string) (string, bool) {
		if k == "API_KEY" {
			return "s3cr3t", true
		}
		return "", false
	}
	got, err := resolveEnv([]string{"API_KEY", "MODE=dev"}, []string{f}, lookup, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"DB_URL=postgres://x", "TOKEN=a=b", "API_KEY=s3cr3t", "MODE=dev"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveEnv = %q, want %q", got, want)
	}

	if _, err := resolveEnv([]string{"NO_ESTA"}, nil, lookup, os.ReadFile); err == nil {
		t.Error("-e with an unset variable was accepted")
	}
	if _, err := resolveEnv([]string{"=x"}, nil, lookup, os.ReadFile); err == nil {
		t.Error("-e =x was accepted")
	}
	mala := filepath.Join(dir, "mala")
	if err := os.WriteFile(mala, []byte("OK=1\nesto-es-un-secreto\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = resolveEnv(nil, []string{mala}, lookup, os.ReadFile)
	if err == nil || strings.Contains(err.Error(), "esto-es-un-secreto") || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("bad env file: err = %v (must name the line, not its content)", err)
	}
}

// cmdExec acepta -e KEY (antes: "-e \"KEY\": use KEY=value", sin llegar al daemon).
func TestExecAceptaEKey(t *testing.T) {
	t.Setenv("KLING_TEST_SECRETO", "x")
	_, err := cmdExec([]string{"-H", filepath.Join(t.TempDir(), "no.sock"), "-e", "KLING_TEST_SECRETO", "m", "true"})
	if err != nil && strings.Contains(err.Error(), "use KEY=value") {
		t.Fatalf("-e KEY rejected: %v", err)
	}
}
