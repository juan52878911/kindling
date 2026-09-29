//go:build linux

package machine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/pkg/digest"
)

// imagenFalsa es un directorio que hace de imagen montada, y otro FUERA de
// ella que hace del host.
func imagenFalsa(t *testing.T) (mnt, fuera string) {
	t.Helper()
	mnt, fuera = t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(mnt, "usr"), 0o755); err != nil {
		t.Fatal(err)
	}
	return mnt, fuera
}

func fuente(t *testing.T, contenido string) (string, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(p, []byte(contenido), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := digest.File(p)
	if err != nil {
		t.Fatal(err)
	}
	return p, d
}

func vacio(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("se escribió FUERA de la imagen, en %s: %v", dir, ents)
	}
}

// La fuga: usr/local -> /tmp/<fuera> en la imagen. Antes, poner
// /usr/local/bin/x escribía <fuera>/bin/x en el host, como root.
func TestPonerEnImagenEnlaceAbsolutoNoSale(t *testing.T) {
	mnt, fuera := imagenFalsa(t)
	if err := os.Symlink(fuera, filepath.Join(mnt, "usr", "local")); err != nil {
		t.Fatal(err)
	}
	src, d := fuente(t, "puente")
	cambio, err := ponerEnImagen(mnt, "/usr/local/bin/x", src, d, 0o755, true)
	if err != nil || !cambio {
		t.Fatalf("cambio=%v err=%v", cambio, err)
	}
	vacio(t, fuera)
	// El destino absoluto se toma desde la raíz de la imagen.
	dentro := filepath.Join(mnt, fuera, "bin", "x")
	b, err := os.ReadFile(dentro)
	if err != nil || string(b) != "puente" {
		t.Fatalf("no acabó dentro de la imagen (%s): %q %v", dentro, b, err)
	}
	if fi, _ := os.Stat(dentro); fi.Mode().Perm() != 0o755 {
		t.Fatalf("modo %v; quiero 0755 exacto", fi.Mode().Perm())
	}
}

// Un ".." no pasa de la raíz de la imagen.
func TestPonerEnImagenPuntosNoSalen(t *testing.T) {
	mnt, fuera := imagenFalsa(t)
	rel, err := filepath.Rel(filepath.Join(mnt, "usr"), fuera)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, filepath.Join(mnt, "usr", "local")); err != nil {
		t.Fatal(err)
	}
	src, d := fuente(t, "x")
	if _, err := ponerEnImagen(mnt, "/usr/local/x", src, d, 0o644, true); err != nil {
		t.Fatal(err)
	}
	vacio(t, fuera)
}

// El último componente no se sigue: un enlace a un fichero del host se
// reemplaza por el fichero, y el del host no se toca.
func TestPonerEnImagenUltimoEnlaceNoSeSigue(t *testing.T) {
	mnt, fuera := imagenFalsa(t)
	victima := filepath.Join(fuera, "shadow")
	if err := os.WriteFile(victima, []byte("secreto"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victima, filepath.Join(mnt, "usr", "f")); err != nil {
		t.Fatal(err)
	}
	src, d := fuente(t, "nuevo")
	if cambio, err := ponerEnImagen(mnt, "/usr/f", src, d, 0o644, false); err != nil || !cambio {
		t.Fatalf("cambio=%v err=%v", cambio, err)
	}
	if b, _ := os.ReadFile(victima); string(b) != "secreto" {
		t.Fatalf("el fichero del host cambió: %q", b)
	}
	fi, err := os.Lstat(filepath.Join(mnt, "usr", "f"))
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("el enlace no se reemplazó por un fichero: %v %v", fi, err)
	}
	// Y el .nuevo de un intento anterior que fuera un enlace al host se
	// borra sin seguirlo.
	if err := os.Symlink(victima, filepath.Join(mnt, "usr", "g.nuevo")); err != nil {
		t.Fatal(err)
	}
	if _, err := ponerEnImagen(mnt, "/usr/g", src, d, 0o644, true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victima); string(b) != "secreto" {
		t.Fatalf("el fichero del host cambió por el .nuevo: %q", b)
	}
}

func TestPonerEnImagenContrato(t *testing.T) {
	mnt, _ := imagenFalsa(t)
	src, d := fuente(t, "a")
	if _, err := ponerEnImagen(mnt, "/usr/bin/x", src, d, 0o644, false); !errors.Is(err, errNoBridge) {
		t.Fatalf("sin create y sin directorio: errNoBridge; fue %v", err)
	}
	if cambio, err := ponerEnImagen(mnt, "/usr/bin/x", src, d, 0o644, true); err != nil || !cambio {
		t.Fatalf("crear: cambio=%v err=%v", cambio, err)
	}
	if cambio, err := ponerEnImagen(mnt, "/usr/bin/x", src, d, 0o644, false); err != nil || cambio {
		t.Fatalf("idéntico: cambio=%v err=%v", cambio, err)
	}
	if _, err := ponerEnImagen(mnt, "/usr/bin/x/y", src, d, 0o644, true); err == nil {
		t.Fatal("un fichero no hace de directorio")
	}
	if _, err := ponerEnImagen(mnt, "/usr", src, d, 0o644, true); err == nil {
		t.Fatal("sobre un directorio no se escribe")
	}
	// Un bucle de enlaces se corta.
	if err := os.Symlink("bucle", filepath.Join(mnt, "bucle")); err != nil {
		t.Fatal(err)
	}
	if _, err := ponerEnImagen(mnt, "/bucle/x", src, d, 0o644, true); err == nil {
		t.Fatal("un bucle de enlaces no termina en nada")
	}
}
