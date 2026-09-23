package fc

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Un socket con una ruta más larga que sun_path se alcanza por el enlace
// corto. El servidor se ata como kling-vz: chdir y nombre corto.
func TestDialSocketConRutaLarga(t *testing.T) {
	base, err := os.MkdirTemp("", "fcl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	dir := filepath.Join(base, strings.Repeat("a", 60), strings.Repeat("b", 40))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "fc.sock")
	if len(sock) < maxSunPath {
		t.Fatalf("la ruta de prueba no es larga: %d", len(sock))
	}
	cwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", "fc.sock")
	_ = os.Chdir(cwd)
	if err != nil {
		t.Skipf("sin sockets unix: %v", err)
	}
	defer ln.Close()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"footprint_mib": 7}`)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	c := New(sock)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping por la ruta larga: %v", err)
	}
	if n, err := c.KlingFootprintMiB(context.Background()); err != nil || n != 7 {
		t.Fatalf("footprint = %d, %v", n, err)
	}
	// El enlace se reutiliza: una segunda conexión no falla por EEXIST.
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEnlaceCortoRechazaDirectorioAjeno(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "abierto")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := enlaceCorto(dir, "/x/fc.sock"); err == nil {
		t.Fatal("un directorio con permisos para otros no vale")
	}
	priv := filepath.Join(t.TempDir(), "priv")
	a, err := enlaceCorto(priv, "/x/uno.sock")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := enlaceCorto(priv, "/x/dos.sock")
	if a == b || len(filepath.Join(dirEnlaces(), filepath.Base(a))) >= maxSunPath {
		t.Fatalf("enlaces %q y %q", a, b)
	}
	if dst, _ := os.Readlink(a); dst != "/x/uno.sock" {
		t.Fatalf("destino = %q", dst)
	}
}
