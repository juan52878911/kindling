//go:build darwin

package machine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// El put no edita la imagen en su sitio: debugfs no tiene rename, y un corte
// entre su rm y su ln dejaba la imagen sin kling-bridge. Se edita un clon y el
// clon sustituye a la imagen con un rename durable: o la vieja entera o la
// nueva entera. Se ve en que un enlace duro a la imagen de antes conserva el
// contenido viejo (su inodo no se tocó) y en que la imagen nueva tiene el
// fichero nuevo y el modo de la vieja.
func TestPutDebugfsNoEditaLaImagenEnSuSitio(t *testing.T) {
	viejo, _ := fichero(t, "viejo\n")
	m, im := ext4Prueba(t, 16, "mkdir /sbin", "cd /sbin", "write "+comillas(viejo)+" kling-bridge")
	if err := os.Chmod(im.file, 0o640); err != nil {
		t.Fatal(err)
	}
	antes := filepath.Join(filepath.Dir(im.file), "antes.ext4")
	if err := os.Link(im.file, antes); err != nil {
		t.Fatal(err)
	}
	src, d := fichero(t, "nuevo\n")
	cambio, err := m.intentarPutDebugfs(context.Background(), im.file, "/sbin/kling-bridge", src, d, 0o755, false)
	if err != nil || !cambio {
		t.Fatalf("put: cambio=%v err=%v", cambio, err)
	}
	if got := leerEnImagen(t, im, "/sbin/kling-bridge"); got != "nuevo\n" {
		t.Fatalf("kling-bridge = %q", got)
	}
	if got := leerEnImagen(t, imagenDebugfs{bin: im.bin, file: antes}, "/sbin/kling-bridge"); got != "viejo\n" {
		t.Fatalf("la imagen de antes se editó en su sitio: kling-bridge = %q", got)
	}
	fi, err := os.Stat(im.file)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("modo de la imagen %o, quería 0640", fi.Mode().Perm())
	}
	if ms, _ := filepath.Glob(filepath.Join(filepath.Dir(im.file), "*.put-*")); len(ms) != 0 {
		t.Fatalf("quedaron temporales: %v", ms)
	}
	fsckLimpio(t, im)
}

// Un put que falla deja la imagen como estaba, sin temporales.
func TestPutDebugfsQueFallaNoTocaLaImagen(t *testing.T) {
	viejo, _ := fichero(t, "viejo\n")
	m, im := ext4Prueba(t, 16, "mkdir /sbin", "cd /sbin", "write "+comillas(viejo)+" kling-bridge")
	antes := filepath.Join(filepath.Dir(im.file), "antes.ext4")
	if err := os.Link(im.file, antes); err != nil {
		t.Fatal(err)
	}
	src, _ := fichero(t, "nuevo\n")
	// Un sha que no cuadra: la copia se escribe y la comprobación falla.
	if _, err := m.intentarPutDebugfs(context.Background(), im.file, "/sbin/kling-bridge", src, "00", 0o755, false); err == nil {
		t.Fatal("el put con un sha que no cuadra no falló")
	}
	a, _ := os.Stat(im.file)
	b, _ := os.Stat(antes)
	if !os.SameFile(a, b) {
		t.Fatal("un put fallido sustituyó la imagen")
	}
	if ms, _ := filepath.Glob(filepath.Join(filepath.Dir(im.file), "*.put-*")); len(ms) != 0 {
		t.Fatalf("quedaron temporales: %v", ms)
	}
}
