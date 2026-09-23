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
	// Al acabar, fuera también su enlace en /tmp/kling-<uid>: si no, cada
	// pasada de los tests dejaba uno colgando.
	defer BarrerEnlaces(base)
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

// Solo se barren los enlaces de esta raíz cuyo directorio ya no existe: el de
// una máquina congelada (directorio vivo, sin socket) se queda.
func TestBarrerEnlacesSoloLosDeMaquinasBorradas(t *testing.T) {
	links := t.TempDir()
	raiz := t.TempDir()
	viva := filepath.Join(raiz, "machines", "viva")
	if err := os.MkdirAll(viva, 0o755); err != nil {
		t.Fatal(err)
	}
	mk := func(nombre, destino string) string {
		p := filepath.Join(links, nombre)
		if err := os.Symlink(destino, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	borrada := mk("a.sock", filepath.Join(raiz, "machines", "borrada", "fc.sock"))
	congelada := mk("b.sock", filepath.Join(viva, "fc.sock"))
	ajena := mk("c.sock", "/otra/raiz/machines/x/fc.sock")
	barrerEnlaces(links, raiz)
	if _, err := os.Lstat(borrada); !os.IsNotExist(err) {
		t.Fatal("el enlace de una máquina borrada sigue ahí")
	}
	for _, p := range []string{congelada, ajena} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s no debía borrarse: %v", p, err)
		}
	}
}
