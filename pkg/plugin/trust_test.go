//go:build unix

package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// markedPlugin escribe un kling-<name> que deja una marca si alguien lo ejecuta.
func markedPlugin(t *testing.T, d, name string) (bin, marca string) {
	t.Helper()
	bin = filepath.Join(d, "kling-"+name)
	marca = bin + ".ran"
	body := "#!/bin/sh\ntouch '" + marca + "'\necho '{\"manifest_version\":1,\"name\":\"" + name + "\",\"version\":\"1\",\"commands\":[{\"name\":\"" + name + "\"}]}'\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, marca
}

func discoverIn(path ...string) *Registry {
	return Discover(context.Background(), Options{Version: "0.5.0", Path: path})
}

func TestNoEjecutaLoQueOtroPuedeCambiar(t *testing.T) {
	t.Run("binario con escritura de grupo", func(t *testing.T) {
		d := t.TempDir()
		bin, marca := markedPlugin(t, d, "grupo")
		os.Chmod(bin, 0o775)
		p := find(discoverIn(d), "grupo")
		if p == nil || p.Err == nil || !strings.Contains(p.Err.Error(), "not run") {
			t.Fatalf("esperaba 'not run', hay %+v", p)
		}
		if _, err := os.Stat(marca); err == nil {
			t.Fatal("se ejecutó un binario con escritura de grupo")
		}
	})
	t.Run("directorio con escritura para todos", func(t *testing.T) {
		d := t.TempDir()
		_, marca := markedPlugin(t, d, "abierto")
		os.Chmod(d, 0o777)
		p := find(discoverIn(d), "abierto")
		if p == nil || p.Err == nil || !strings.Contains(p.Err.Error(), "writable") {
			t.Fatalf("esperaba 'writable', hay %+v", p)
		}
		if _, err := os.Stat(marca); err == nil {
			t.Fatal("se ejecutó un binario de un directorio con escritura para todos")
		}
	})
	t.Run("enlace a un directorio abierto", func(t *testing.T) {
		abierto, d := t.TempDir(), t.TempDir()
		bin, marca := markedPlugin(t, abierto, "enlace")
		os.Chmod(abierto, 0o777)
		if err := os.Symlink(bin, filepath.Join(d, "kling-enlace")); err != nil {
			t.Fatal(err)
		}
		p := find(discoverIn(d), "enlace")
		if p == nil || p.Err == nil {
			t.Fatalf("esperaba un error, hay %+v", p)
		}
		if _, err := os.Stat(marca); err == nil {
			t.Fatal("se siguió el enlace hasta un directorio con escritura para todos")
		}
	})
	t.Run("el mío sí", func(t *testing.T) {
		d := t.TempDir()
		_, marca := markedPlugin(t, d, "mio")
		p := find(discoverIn(d), "mio")
		if p == nil || p.Err != nil {
			t.Fatalf("una extensión propia y sin escritura ajena debería cargar: %+v", p)
		}
		if _, err := os.Stat(marca); err != nil {
			t.Fatal("no se le pidió el manifiesto")
		}
	})
}

func TestIgnoraEntradasRelativas(t *testing.T) {
	d := t.TempDir()
	markedPlugin(t, d, "relativa")
	t.Chdir(d)
	if p := find(discoverIn("", ".", "./"), "relativa"); p != nil {
		t.Fatalf("una entrada relativa no debería contar: %+v", p)
	}
	t.Setenv("KLING_PLUGIN_PATH", "")
	t.Setenv("PATH", "::.:bin:"+d)
	for _, s := range SearchPath() {
		if !filepath.IsAbs(s) {
			t.Fatalf("SearchPath devuelve una entrada relativa: %q", s)
		}
	}
}

func TestSalidaConTope(t *testing.T) {
	d := t.TempDir()
	bin := filepath.Join(d, "kling-grande")
	// Más de 1 MiB tanto en --kling-manifest como en un gancho.
	body := "#!/bin/sh\ndd if=/dev/zero bs=1024 count=2048 2>/dev/null\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runManifest(context.Background(), bin); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("esperaba el error del tope, hay %v", err)
	}
	out, err := RunHook(context.Background(), &Plugin{Name: "grande", Path: bin}, HookStatus, nil, "")
	if err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("esperaba el error del tope, hay %v", err)
	}
	if len(out) != maxHookBytes {
		t.Fatalf("guardó %d bytes, el tope es %d", len(out), maxHookBytes)
	}
}
