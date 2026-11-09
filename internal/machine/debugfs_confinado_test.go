package machine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

// resize2fs y debugfs leen, como e2fsck, un ext4 que pudo escribir un
// invitado (A1): todos pasan por el confinamiento, con el disco por
// descriptor y sin su ruta en la línea de órdenes.
func TestResize2fsYDebugfsPasanPorElConfinamiento(t *testing.T) {
	_, im := ext4Prueba(t, 16, "mkdir etc")
	if buscarE2fs("resize2fs") == "" {
		t.Skip("sin resize2fs")
	}
	var mu sync.Mutex
	vistos := map[string][]string{}
	antes := confinarDisco
	confinarDisco = func(c *exec.Cmd, f *os.File) (func(), error) {
		mu.Lock()
		vistos[filepath.Base(c.Path)] = append([]string(nil), c.Args...)
		mu.Unlock()
		return antes(c, f)
	}
	t.Cleanup(func() { confinarDisco = antes })

	ctx := context.Background()
	if _, err := hasFile(ctx, im.file, "/etc"); err != nil {
		t.Fatal(err)
	}
	if _, err := im.stat(ctx, "/etc"); err != nil {
		t.Fatal(err)
	}
	quitarLostFound(ctx, im.file)
	if err := crecerImagen(ctx, im.file, 4<<20); err != nil {
		t.Fatal(err)
	}
	for _, prog := range []string{"debugfs", "resize2fs", "e2fsck"} {
		args, ok := vistos[prog]
		if !ok {
			t.Errorf("%s no pasó por el confinamiento (vistos: %v)", prog, vistos)
			continue
		}
		if !slices.Contains(args, rutaE2fsck) || strings.Contains(strings.Join(args, " "), im.file) {
			t.Errorf("%s no recibe el disco por descriptor: %q", prog, args)
		}
	}
}

// El debugfs confinado no lee ficheros del host por su ruta: una orden write
// que apunta a uno (un guion hecho con una ruta que no toca) falla, y la
// imagen no se lo lleva. En Linux hace falta root (sin él no hay uid que
// bajar); en macOS es el sandbox.
func TestDebugfsConfinadoNoLeeElHost(t *testing.T) {
	if runtime.GOOS == "linux" && os.Geteuid() != 0 {
		t.Skip("en Linux hace falta root para bajar el uid de debugfs")
	}
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
			t.Skip("sin sandbox-exec")
		}
	}
	_, im := ext4Prueba(t, 16)
	dir := t.TempDir()
	// Atravesable por cualquiera: si no lo lee es por el confinamiento.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	secreto := filepath.Join(dir, "secreto")
	if err := os.WriteFile(secreto, []byte("no-lo-leas"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := im.escribir(ctx, []string{"write " + comillas(secreto) + " robado"}); err == nil {
		t.Error("debugfs confinado copió a la imagen un fichero del host por su ruta")
	}
	e, err := im.stat(ctx, "/robado")
	if err != nil {
		t.Fatal(err)
	}
	if e.existe {
		if b, _ := im.leerFichero(ctx, "/robado", 64); len(b) > 0 {
			t.Fatalf("la imagen se llevó el fichero del host: %q", b)
		}
	}
	// Y con la fuente abierta por el daemon (como put), sí.
	f, err := os.Open(secreto)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := im.escribir(ctx, []string{"write " + rutaFuentePut + " copiado"}, f); err != nil {
		t.Fatal(err)
	}
	if b, err := im.leerFichero(ctx, "/copiado", 64); err != nil || string(b) != "no-lo-leas" {
		t.Fatalf("la copia por descriptor: %q %v", b, err)
	}
}
