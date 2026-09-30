package machine

import (
	"context"
	"os"
	"os/exec"
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

// borrarJail desmonta todo lo que haya bajo el jail, no solo el bind en su
// ruta original, y lo más hondo primero.
func TestMontajesBajo(t *testing.T) {
	info := strings.Join([]string{
		"20 1 8:1 / / rw - ext4 /dev/sda1 rw",
		"30 20 0:50 / /var/lib/kindling/cow rw - xfs /dev/loop0 rw",
		"31 20 0:50 /cow/ab /var/lib/kindling/jails/firecracker/ab/root/movido/lib/kindling/cow/ab rw - xfs /dev/loop0 rw",
		"32 31 0:51 / /var/lib/kindling/jails/firecracker/ab/root/movido/lib/kindling/cow/ab/x rw - tmpfs tmpfs rw",
		"33 20 0:52 / /var/lib/kindling/jails/firecracker/abc/root rw - tmpfs tmpfs rw",
		"34 20 0:53 / /var/lib/kindling/jails/firecracker/ab rw - tmpfs tmpfs rw",
	}, "\n")
	ms, err := parsearMountinfo(strings.NewReader(info))
	if err != nil {
		t.Fatal(err)
	}
	got := montajesBajo(ms, "/var/lib/kindling/jails/firecracker/ab/")
	want := []string{
		"/var/lib/kindling/jails/firecracker/ab/root/movido/lib/kindling/cow/ab/x",
		"/var/lib/kindling/jails/firecracker/ab/root/movido/lib/kindling/cow/ab",
		"/var/lib/kindling/jails/firecracker/ab",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("montajesBajo = %q, quería %q (sin el jail de abc)", got, want)
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

// Lo que el VMM dejó en su chroot solo se recupera si es el fichero que él
// escribió: ni un enlace simbólico ni un hardlink a un fichero del host, ni a
// través de un directorio cambiado por un enlace.
func TestRecuperarDelJail(t *testing.T) {
	uid := os.Geteuid()
	preparar := func(t *testing.T) (raiz, dst, secreto string) {
		raiz, dst = t.TempDir(), t.TempDir()
		secreto = filepath.Join(t.TempDir(), "secreto")
		if err := os.WriteFile(secreto, []byte("del host"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(raiz, "snap.file"), []byte("estado"), 0o600); err != nil {
			t.Fatal(err)
		}
		return raiz, dst, secreto
	}
	intacto := func(t *testing.T, secreto string) {
		t.Helper()
		if b, err := os.ReadFile(secreto); err != nil || string(b) != "del host" {
			t.Fatalf("el fichero del host cambió: %q, %v", b, err)
		}
	}

	t.Run("el fichero del VMM", func(t *testing.T) {
		raiz, dst, _ := preparar(t)
		if err := os.WriteFile(filepath.Join(raiz, "mem.file"), []byte("ram"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := recuperarDelJail(raiz, "/", dst, uid, "snap.file", "mem.file"); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(filepath.Join(dst, "mem.file")); err != nil || string(b) != "ram" {
			t.Fatalf("mem.file recuperado: %q, %v", b, err)
		}
	})

	t.Run("enlace simbolico", func(t *testing.T) {
		raiz, dst, secreto := preparar(t)
		if err := os.Symlink(secreto, filepath.Join(raiz, "mem.file")); err != nil {
			t.Fatal(err)
		}
		if err := recuperarDelJail(raiz, "/", dst, uid, "snap.file", "mem.file"); err == nil {
			t.Fatal("recuperó un enlace simbólico como mem.file")
		}
		if _, err := os.Lstat(filepath.Join(dst, "mem.file")); !os.IsNotExist(err) {
			t.Fatalf("el enlace se quedó en el directorio del host: %v", err)
		}
		intacto(t, secreto)
	})

	t.Run("hardlink", func(t *testing.T) {
		raiz, dst, secreto := preparar(t)
		if err := os.Link(secreto, filepath.Join(raiz, "mem.file")); err != nil {
			t.Skip(err) // otro sistema de ficheros
		}
		if err := recuperarDelJail(raiz, "/", dst, uid, "snap.file", "mem.file"); err == nil {
			t.Fatal("recuperó un hardlink a un fichero del host como mem.file")
		}
		if _, err := os.Lstat(filepath.Join(dst, "mem.file")); !os.IsNotExist(err) {
			t.Fatalf("el hardlink se quedó en el directorio del host: %v", err)
		}
		intacto(t, secreto)
	})

	t.Run("de otro usuario", func(t *testing.T) {
		raiz, dst, _ := preparar(t)
		if err := recuperarDelJail(raiz, "/", dst, uid+1, "snap.file"); err == nil {
			t.Fatal("recuperó un fichero que no es del usuario del VMM")
		}
	})

	t.Run("directorio cambiado por un enlace", func(t *testing.T) {
		raiz, dst, secreto := preparar(t)
		fuera := filepath.Dir(secreto)
		if err := os.WriteFile(filepath.Join(fuera, "snap.file"), []byte("del host"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(fuera, filepath.Join(raiz, "var")); err != nil {
			t.Fatal(err)
		}
		if err := recuperarDelJail(raiz, "/var", dst, uid, "snap.file"); err == nil {
			t.Fatal("recuperó a través de un directorio cambiado por un enlace")
		}
		if b, err := os.ReadFile(filepath.Join(fuera, "snap.file")); err != nil || string(b) != "del host" {
			t.Fatalf("movió un fichero del host: %q, %v", b, err)
		}
	})
}

// Commit de una plantilla jailed cuyo VMM deja como mem.file un enlace a un
// fichero del host. Antes el rename lo traía tal cual al dorado, y el daemon
// lo perforaba (fallocate como root, a través del enlace), lo hasheaba y lo
// daba por bueno.
func TestCommitJailedNoRecuperaUnEnlaceComoVolcado(t *testing.T) {
	if _, err := exec.LookPath("fallocate"); err != nil {
		t.Skip("sin fallocate no se puede perforar el volcado")
	}
	m := newTestManager(t)
	id := "c0aa1700000000e2"
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

	secreto := filepath.Join(t.TempDir(), "secreto")
	if err := os.WriteFile(secreto, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := m.snapDir("dorado")
	falso.enGancho(func(metodo, ruta string) {
		if ruta == "/snapshot/create" {
			// Lo que escribiría un Firecracker comprometido en su chroot.
			_ = os.WriteFile(m.jailPath(id, filepath.Join(dir, "snap.file")), []byte("estado"), 0o644)
			_ = os.Symlink(secreto, m.jailPath(id, filepath.Join(dir, "mem.file")))
		}
	})

	if _, err := m.Commit(context.Background(), id, "dorado", false); err == nil {
		t.Fatal("Commit aceptó como mem.file un enlace plantado por el VMM")
	}
	if _, err := os.Lstat(filepath.Join(dir, "mem.file")); !os.IsNotExist(err) {
		t.Fatalf("el enlace quedó en el dorado: %v", err)
	}
}
