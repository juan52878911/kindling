package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/plugin"
)

// El binario de test hace de kling-hello cuando lleva esta variable: así el
// test ejecuta la extensión de verdad (plugin.Main llama a os.Exit) sin tener
// que compilarla aparte.
const asExtension = "KLING_HELLO_AS_EXTENSION"

func TestMain(m *testing.M) {
	if os.Getenv(asExtension) == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func run(t *testing.T, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(append(os.Environ(), asExtension+"=1"), env...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), code
}

func TestManifiesto(t *testing.T) {
	out, code := run(t, nil, "--kling-manifest")
	if code != 0 {
		t.Fatalf("--kling-manifest: exit %d\n%s", code, out)
	}
	var m plugin.Manifest
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("manifest is not JSON: %v\n%s", err, out)
	}
	// Es lo que hará kling al descubrirla: si no valida, no se ejecuta.
	if err := m.Validate(); err != nil {
		t.Fatalf("invalid manifest: %v", err)
	}
	want := manifest
	want.ManifestVersion = plugin.ManifestVersion
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("printed manifest differs:\n got %+v\nwant %+v", m, want)
	}
}

func TestSaludo(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	env := []string{"KLING_CONFIG=" + cfg}

	if out, code := run(t, env, "hello", "-name", "Ada"); code != 0 || out != "hello, Ada!\n" {
		t.Fatalf("hello: exit %d, %q", code, out)
	}
	if err := os.WriteFile(cfg, []byte(`{"extensions":{"hello":{"greeting":"hola"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, _ := run(t, env, "hello"); out != "hola, world!\n" {
		t.Fatalf("greeting from config: %q", out)
	}
	if out, code := run(t, env, "--kling-hook", "status"); code != 0 || !strings.Contains(out, `"hola"`) {
		t.Fatalf("status hook: exit %d, %q", code, out)
	}
	if _, code := run(t, env, "hello", "-nope"); code != 2 {
		t.Fatalf("bad flag: exit %d, want 2", code)
	}
}
