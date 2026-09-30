package machine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jailFalso deja en raiz/<c1> un enlace simbólico a fuera, como el que
// plantaría un VMM en la raíz de su chroot (que es suya), y devuelve la ruta
// del host a la que llevaría dir si se siguiera.
func enlacePlantado(t *testing.T, raiz, dir string) (fuera, destino string) {
	t.Helper()
	c1, resto, _ := strings.Cut(strings.TrimPrefix(filepath.Clean(dir), "/"), "/")
	fuera = filepath.Join(t.TempDir(), "host")
	if err := os.MkdirAll(raiz, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fuera, filepath.Join(raiz, c1)); err != nil {
		t.Skip(err)
	}
	return fuera, filepath.Join(fuera, resto)
}

func TestAbrirDirSinEnlacesCreaLoQueFalta(t *testing.T) {
	raiz := t.TempDir()
	d, err := abrirDirSinEnlaces(raiz, "/var/lib/kindling/snapshots/x", true)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	if fi, err := os.Lstat(filepath.Join(raiz, "var/lib/kindling/snapshots/x")); err != nil || !fi.IsDir() {
		t.Fatalf("no creó el directorio: %v", err)
	}
	// Un ".." no sale de la raíz.
	d, err = abrirDirSinEnlaces(raiz, "/../../x", true)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	if _, err := os.Lstat(filepath.Join(raiz, "x")); err != nil {
		t.Fatalf(`"/../../x" no quedó dentro de la raíz: %v`, err)
	}
}

// El VMM cambia un directorio de la ruta por un enlace al host: ni se sigue
// al crear ni al abrir.
func TestAbrirDirSinEnlacesNoSigueUnEnlacePlantado(t *testing.T) {
	raiz := t.TempDir()
	dir := "/var/lib/kindling/snapshots/dorado"
	_, destino := enlacePlantado(t, raiz, dir)
	if err := os.MkdirAll(filepath.Dir(destino), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, crear := range []bool{true, false} {
		if d, err := abrirDirSinEnlaces(raiz, dir, crear); err == nil {
			d.Close()
			t.Fatalf("crear=%v: abrió %s a través de un enlace plantado", crear, dir)
		}
	}
	if _, err := os.Lstat(destino); !os.IsNotExist(err) {
		t.Fatalf("se creó %s fuera del jail: %v", destino, err)
	}
}

// Borrar un árbol del jail no borra lo que haya al otro lado de un enlace,
// ni en un componente intermedio ni dentro del árbol.
func TestBorrarEnJailNoSigueEnlaces(t *testing.T) {
	raiz := t.TempDir()
	dir := "/var/lib/kindling/snapshots/dorado"
	_, destino := enlacePlantado(t, raiz, dir)
	canario := filepath.Join(destino, "canario")
	if err := os.MkdirAll(destino, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canario, []byte("del host"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = borrarEnJail(raiz, dir)
	if _, err := os.Stat(canario); err != nil {
		t.Fatalf("borrarEnJail siguió el enlace del componente intermedio: %v", err)
	}

	// Y dentro del árbol: un enlace a un fichero y otro a un directorio del
	// host se borran como enlaces.
	raiz2 := t.TempDir()
	arbol := filepath.Join(raiz2, "a", "b")
	if err := os.MkdirAll(filepath.Join(arbol, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canario, filepath.Join(arbol, "f")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(destino, filepath.Join(arbol, "sub", "d")); err != nil {
		t.Fatal(err)
	}
	if err := borrarEnJail(raiz2, "/a/b"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(arbol); !os.IsNotExist(err) {
		t.Fatalf("no borró el árbol: %v", err)
	}
	if _, err := os.Stat(canario); err != nil {
		t.Fatalf("borró el destino de un enlace: %v", err)
	}
	if err := borrarEnJail(raiz2, "/a/b"); err != nil {
		t.Fatalf("lo que ya no existe no es un error: %v", err)
	}
	if err := borrarEnJail(raiz2, "/"); err == nil {
		t.Fatal("borró la raíz del jail")
	}
}

// Commit de una plantilla jailed cuyo VMM cambió un directorio de la réplica
// de snapshots/ en su chroot por un enlace al host. Antes, el MkdirAll (y el
// Chown) de la réplica, el overlay dorado y el RemoveAll de la limpieza
// seguían ese enlace, como root: creaban y borraban en el host.
func TestCommitJailedNoSigueEnlacesDelChroot(t *testing.T) {
	m := newTestManager(t)
	id := "c0aa1700000000e1"
	falso, _ := plantillaParaCommit(t, m, id)
	m.jailerJailed = true
	if err := os.MkdirAll(filepath.Dir(m.jailSock(id)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(falso.Sock, m.jailSock(id)); err != nil {
		t.Skip(err)
	}
	m.mu.Lock()
	m.socket[id] = m.jailSock(id)
	m.mu.Unlock()

	dir := m.snapDir("dorado")
	_, destino := enlacePlantado(t, filepath.Join(m.jailRoot(id)), dir)
	canario := filepath.Join(destino, "canario")
	if err := os.MkdirAll(destino, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canario, []byte("del host"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Commit(context.Background(), id, "dorado", false); err == nil {
		t.Fatal("Commit siguió adelante con la réplica del directorio a través de un enlace")
	}
	if _, err := os.Stat(canario); err != nil {
		t.Fatalf("la limpieza del commit borró en el host a través del enlace: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(destino, "overlay.ext4")); !os.IsNotExist(err) {
		t.Fatalf("el overlay dorado se escribió en el host a través del enlace: %v", err)
	}
}
