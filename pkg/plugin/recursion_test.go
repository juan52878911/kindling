package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Una copia de kling llamada kling-algo en el PATH no puede hacer que cada
// `kling …` se lance a sí mismo sin fin: la extensión recibe EnvManifest
// mientras contesta a --kling-manifest, y un Discover que lo ve (es ese
// kling) no descubre nada.
func TestManifestNoRecursivo(t *testing.T) {
	d := t.TempDir()
	// La "extensión" apunta qué entorno ve al contestar.
	script := "#!/bin/sh\nprintf '%s' \"$" + EnvManifest + "\" > \"$0.env\"\necho '{\"manifest_version\":1,\"name\":\"eco\",\"version\":\"1.0.0\",\"commands\":[{\"name\":\"eco\"}]}'\n"
	path := filepath.Join(d, "kling-eco")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := Discover(context.Background(), Options{Version: "1.0.0", Path: []string{d}})
	if p := find(r, "eco"); p == nil || p.Err != nil {
		t.Fatalf("kling-eco not discovered: %+v", p)
	}
	got, _ := os.ReadFile(path + ".env")
	if string(got) != path {
		t.Fatalf("%s while answering --kling-manifest = %q, want %q", EnvManifest, got, path)
	}
	t.Setenv(EnvManifest, path)
	if r := Discover(context.Background(), Options{Version: "1.0.0", Path: []string{d}}); len(r.Plugins) != 0 {
		t.Fatalf("a kling answering --kling-manifest discovered %d extensions; it must discover none", len(r.Plugins))
	}
}

// Una extensión que vuelve a lanzar kling (un `kling foo` dentro de foo, un
// gancho de status que llama a `kling status`) no puede anidarse sin fin: cada
// nivel lleva EnvDepth+1 y, al llegar a MaxDepth, Discover ya no descubre.
func TestProfundidadCortaLaRecursion(t *testing.T) {
	d := t.TempDir()
	script := "#!/bin/sh\necho '{\"manifest_version\":1,\"name\":\"eco\",\"version\":\"1.0.0\",\"commands\":[{\"name\":\"eco\"}]}'\n"
	if err := os.WriteFile(filepath.Join(d, "kling-eco"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	nivel := func() string {
		for _, kv := range Env("") {
			if v, ok := strings.CutPrefix(kv, EnvDepth+"="); ok {
				return v
			}
		}
		return ""
	}
	t.Setenv(EnvDepth, "")
	if got := nivel(); got != "1" {
		t.Fatalf("%s for an extension launched by a top-level kling = %q, want 1", EnvDepth, got)
	}
	t.Setenv(EnvDepth, strconv.Itoa(MaxDepth-1))
	if got := nivel(); got != strconv.Itoa(MaxDepth) {
		t.Fatalf("%s one level down from %d = %q, want %d", EnvDepth, MaxDepth-1, got, MaxDepth)
	}
	if r := Discover(context.Background(), Options{Version: "1.0.0", Path: []string{d}}); find(r, "eco") == nil {
		t.Fatalf("at depth %d kling-eco must still be discovered", MaxDepth-1)
	}
	t.Setenv(EnvDepth, strconv.Itoa(MaxDepth))
	if r := Discover(context.Background(), Options{Version: "1.0.0", Path: []string{d}}); len(r.Plugins) != 0 {
		t.Fatalf("at depth %d Discover found %d extensions; it must find none", MaxDepth, len(r.Plugins))
	}
	t.Setenv(EnvDepth, "basura")
	if got := nivel(); got != "1" {
		t.Fatalf("%s from an unreadable value = %q, want 1", EnvDepth, got)
	}
}
