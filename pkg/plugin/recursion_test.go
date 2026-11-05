package plugin

import (
	"context"
	"os"
	"path/filepath"
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
