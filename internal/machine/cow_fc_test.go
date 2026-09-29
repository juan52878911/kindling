//go:build !darwin

package machine

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// FICLONE clona o falla limpio (sin dejar el destino), según el sistema de
// ficheros del directorio temporal; y copiarOverlay en modo reflink cae a la
// copia de siempre cuando no puede clonar.
func TestClonarFicheroOFallaLimpio(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(src, []byte("contenido"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := clonarFichero(src, dst); err != nil {
		if _, serr := os.Lstat(dst); !os.IsNotExist(serr) {
			t.Errorf("un FICLONE fallido (%v) dejó el destino: %v", err, serr)
		}
		if probarReflink(dir, dir) == nil {
			t.Error("la prueba dice que hay reflink y el clon falló")
		}
	} else if b, _ := os.ReadFile(dst); string(b) != "contenido" {
		t.Errorf("clon mal: %q", b)
	}

	m := newTestManager(t)
	m.cow.modo = cowModoReflink
	dst2 := filepath.Join(dir, "c")
	if out, err := m.copiarOverlay(context.Background(), src, dst2); err != nil {
		t.Fatalf("copiarOverlay: %v: %s", err, out)
	}
	if b, _ := os.ReadFile(dst2); string(b) != "contenido" {
		t.Errorf("copiarOverlay: %q", b)
	}
}

// Sin bind del almacén, borrarJail es el RemoveAll de siempre.
func TestBorrarJailSinBind(t *testing.T) {
	m := newTestManager(t)
	m.alm = nuevoAlmacen(m.root, &Privileges{})
	base := filepath.Join(m.jailBase(), "firecracker", "id1")
	if err := os.MkdirAll(filepath.Join(m.jailRoot("id1"), "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	// El directorio del bind existe pero no hay nada montado encima.
	if err := os.MkdirAll(m.dirBindJail("id1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.borrarJail("id1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(base); !os.IsNotExist(err) {
		t.Errorf("el jail sigue: %v", err)
	}
	// Una instancia sin overlay en el almacén no monta nada en su jail.
	if err := m.prepararBindsJail("id2"); err != nil {
		t.Errorf("prepararBindsJail sin almacén: %v", err)
	}
}

// El bind de verdad (root y KLING_TEST_MOUNTS=1; en un contenedor
// --privileged vale): el overlay se ve dentro del jail, y borrar el jail
// desmonta antes de borrar y NO se lleva el overlay del almacén.
func TestBindDelAlmacenEnElJail(t *testing.T) {
	if os.Geteuid() != 0 || os.Getenv("KLING_TEST_MOUNTS") != "1" {
		t.Skip("needs root and KLING_TEST_MOUNTS=1")
	}
	m := newTestManager(t)
	m.alm = nuevoAlmacen(m.root, &Privileges{})
	// Un tmpfs hace de almacén: lo que importa es que es otro dispositivo.
	if err := os.MkdirAll(m.alm.dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mount("tmpfs", m.alm.dir, "tmpfs", 0, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Unmount(m.alm.dir, syscall.MNT_DETACH) })
	d := m.alm.dirInstancia("id1")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(d, "overlay.ext4")
	if err := os.WriteFile(overlay, []byte("disco"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.prepararBindsJail("id1"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(m.jailRoot("id1"), overlay)); err != nil || string(b) != "disco" {
		t.Fatalf("dentro del jail: %q %v", b, err)
	}
	if err := m.borrarJail("id1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(m.jailBase(), "firecracker", "id1")); !os.IsNotExist(err) {
		t.Errorf("el jail sigue: %v", err)
	}
	if b, err := os.ReadFile(overlay); err != nil || string(b) != "disco" {
		t.Fatalf("borrar el jail se llevó el overlay del almacén: %q %v", b, err)
	}
}
