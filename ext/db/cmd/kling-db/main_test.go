package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"testing"

	"github.com/juan52878911/kindling/pkg/plugin"
)

// El binario de test hace de kling-db con esta variable (ver
// examples/hello-extension/main_test.go).
const asExtension = "KLING_DB_AS_EXTENSION"

func TestMain(m *testing.M) {
	if os.Getenv(asExtension) == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func runExt(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), asExtension+"=1", "KLING=/nonexistent/kling")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
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
	out, code := runExt(t, "--kling-manifest")
	if code != 0 {
		t.Fatalf("--kling-manifest: exit %d\n%s", code, out)
	}
	var m plugin.Manifest
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("invalid manifest: %v", err)
	}
	if want := manifest(); !reflect.DeepEqual(m, want) {
		t.Fatalf("printed manifest differs:\n got %+v\nwant %+v", m, want)
	}
}

func TestUsoIncorrecto(t *testing.T) {
	for _, args := range [][]string{
		{"up"},
		{"up", "a", "b"},
		{"connect", "c", "-dsn", "-psql"},
		{"doctor"},
		{"doctor", "c", "-url", "postgres://x"},
		{"audit", "c", "-since", "-1h"},
		{"tenant-check"},
		{"tenant-check", "c", "-max", "0"},
		{"tenant-check", "c", "-setting", "tenant"},
		{"diff", "a"},
		{"diff", "a", "b", "c"},
		{"diff", "a", "b", "-max-rows", "0"},
		{"golden"},
		{"up", "-nope"},
	} {
		if out, code := runExt(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2\n%s", args, code, out)
		}
	}
}
