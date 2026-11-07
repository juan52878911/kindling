package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// dirCorto: t.TempDir() en macOS pasa de los 104 bytes de sun_path con el
// nombre del test.
func dirCorto(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "ks")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// El socket nace 0660, de quien se cede, y en su sitio; el directorio privado
// en el que nace no queda.
func TestEscucharSocketPermisos(t *testing.T) {
	dir := dirCorto(t)
	ruta := filepath.Join(dir, "kling.sock")
	ln, err := escucharSocket(ruta, true, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fi, err := os.Lstat(ruta)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is %v, want a socket", ruta, fi.Mode())
	}
	if p := fi.Mode().Perm(); p != 0o660 {
		t.Fatalf("socket mode %o, want 660", p)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("the socket's directory has %d entries, want only the socket: %v", len(ents), ents)
	}
	// Y se puede conectar por la ruta final.
	c, err := net.Dial("unix", ruta)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

// Quien cambia la ruta del socket por un enlace mientras el daemon arranca no
// consigue que chmod ni chown lleguen al destino del enlace: el socket se
// prepara donde nadie más escribe y el Rename reemplaza el enlace sin seguirlo.
func TestEscucharSocketNoSigueEnlaces(t *testing.T) {
	dir := dirCorto(t)
	ruta := filepath.Join(dir, "kling.sock")
	victima := filepath.Join(dir, "victima")
	if err := os.WriteFile(victima, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	trasEscucharSocket = func(final string) {
		if err := os.Symlink(victima, final); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { trasEscucharSocket = nil })

	ln, err := escucharSocket(ruta, true, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fi, err := os.Stat(victima)
	if err != nil {
		t.Fatal(err)
	}
	if p := fi.Mode().Perm(); p != 0o600 {
		t.Fatalf("the symlink's target changed mode to %o", p)
	}
	li, err := os.Lstat(ruta)
	if err != nil {
		t.Fatal(err)
	}
	if li.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is %v after the swap, want the daemon's socket", ruta, li.Mode())
	}
}

// Al salir, el daemon borra su socket solo si sigue siendo el suyo: si otro
// daemon ya escucha en la misma ruta (un reinicio solapado, un segundo daemon
// con el mismo -socket), no le deja sin socket.
func TestQuitarSocketSoloSiEsElMio(t *testing.T) {
	dir, err := os.MkdirTemp("", "ks")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ruta := filepath.Join(dir, "k.sock")

	ln1, err := escucharSocket(ruta, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln1.Close()
	mio, err := os.Lstat(ruta)
	if err != nil {
		t.Fatal(err)
	}
	// Otro daemon toma la ruta.
	ln2, err := escucharSocket(ruta, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	suyo, _ := os.Lstat(ruta)

	quitarSocketSiEsMio(ruta, mio)
	if fi, err := os.Lstat(ruta); err != nil || !os.SameFile(fi, suyo) {
		t.Fatalf("se borró el socket del otro daemon: %v %v", fi, err)
	}
	quitarSocketSiEsMio(ruta, suyo)
	if _, err := os.Lstat(ruta); !os.IsNotExist(err) {
		t.Fatalf("el socket propio sigue ahí: %v", err)
	}
	// Sin identidad (no se pudo leer al crearlo) no se borra nada.
	quitarSocketSiEsMio(ruta, nil)
}
