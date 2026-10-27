package main

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
)

type klingRec struct{ calls [][]string }

func (k *klingRec) Run(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
	k.calls = append(k.calls, args)
	return nil, nil
}

func TestNombreImagenOCI(t *testing.T) {
	for ref, want := range map[string]string{
		"pgvector/pgvector:pg16":            "dbg-pgvector-pgvector-pg16",
		"docker.io/library/postgres:16":     "dbg-library-postgres-16",
		"ghcr.io/Acme/App-DB@sha256:abcdef": "dbg-ghcr-io-acme-app-db-sha256-abcdef",
	} {
		if got := nombreImagenOCI(ref); got != want {
			t.Errorf("nombreImagenOCI(%q) = %q, want %q", ref, got, want)
		}
	}
	if n := nombreImagenOCI(strings.Repeat("a", 200)); len(n) > 64 {
		t.Errorf("name too long: %d", len(n))
	}
}

func TestImportarOCI(t *testing.T) {
	k := &klingRec{}
	name, err := importarOCI(context.Background(), k, "pgvector/pgvector:pg16")
	if err != nil || name != "dbg-pgvector-pgvector-pg16" {
		t.Fatalf("name=%q err=%v", name, err)
	}
	want := []string{"image", "import", "pgvector/pgvector:pg16", "-name", name, "-restart", "no", "-entrypoint", "sleep", "-entrypoint", "infinity"}
	if len(k.calls) != 1 || !slices.Equal(k.calls[0], want) {
		t.Errorf("calls = %v", k.calls)
	}
	if _, err := importarOCI(context.Background(), k, "--evil"); err == nil {
		t.Error("a ref starting with - must be refused")
	}
}

func TestOCIArgs(t *testing.T) {
	got, err := ociArgs([]string{"build", "-extension", "vector", "mydb"}, "dbg-x")
	want := []string{"build", "-image", "dbg-x", "-loopback", "-extension", "vector", "mydb"}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("got %v, %v", got, err)
	}
	for _, bad := range [][]string{{"image"}, {"build", "-image", "x", "n"}, {"build", "-from=t", "n"}, {"build", "-engine", "mysql", "n"}} {
		if _, err := ociArgs(bad, "dbg-x"); err == nil {
			t.Errorf("ociArgs(%v) should fail", bad)
		}
	}
}
