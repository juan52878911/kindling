package machine

// LO QUE EL DAEMON TOCA DENTRO DEL CHROOT DE UN VMM.
//
// jailer cede al usuario del VMM la raíz de su chroot (<jail>/root). Así que
// todo lo que hay debajo —incluidos los directorios que el daemon creó para
// replicar rutas del host, como /var/lib/kindling/snapshots/<n>— se puede
// renombrar y sustituir por un enlace simbólico desde dentro: basta un
// Firecracker comprometido por su invitado. Y el daemon es root: un
// MkdirAll, un Chown o un RemoveAll por ruta sobre jailPath(...) seguiría ese
// enlace hasta el host y crearía, cedería o borraría lo que el VMM eligiera.
//
// Por eso aquí la ruta se recorre componente a componente desde un descriptor
// de la raíz del jail, con O_NOFOLLOW|O_DIRECTORY: un componente cambiado por
// un enlace falla (ELOOP/ENOTDIR) en vez de seguirse, y lo que se hace al
// final (crear, ceder, mover, borrar) va relativo al descriptor del último
// directorio, sin volver a resolver la ruta.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const oDirSinEnlaces = syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC

// abrirDirSinEnlaces abre el directorio dir (una ruta absoluta del host, como
// todas las del jail) dentro de raiz sin seguir enlaces en ningún componente.
// Con crear, crea (0755) los que falten. Un ".." no pasa de raiz.
func abrirDirSinEnlaces(raiz, dir string, crear bool) (*os.File, error) {
	fd, err := syscall.Open(raiz, oDirSinEnlaces, 0)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", raiz, err)
	}
	visto := "/"
	for _, comp := range strings.Split(filepath.Clean("/"+dir), "/") {
		if comp == "" {
			continue
		}
		sig, err := jOpenat(fd, comp, oDirSinEnlaces, 0)
		if errors.Is(err, syscall.ENOENT) && crear {
			if err := jMkdirat(fd, comp, 0o755); err != nil && !errors.Is(err, syscall.EEXIST) {
				syscall.Close(fd)
				return nil, fmt.Errorf("creating %s in the jail: %w", filepath.Join(visto, comp), err)
			}
			sig, err = jOpenat(fd, comp, oDirSinEnlaces, 0)
		}
		syscall.Close(fd)
		visto = filepath.Join(visto, comp)
		if err != nil {
			return nil, fmt.Errorf("%s in the jail is not a directory the daemon can trust (a symlink?): %w", visto, err)
		}
		fd = sig
	}
	return os.NewFile(uintptr(fd), filepath.Join(raiz, visto)), nil
}

// borrarEnJail borra, sin seguir enlaces ni salirse del sistema de ficheros
// del jail, el árbol dir de dentro de raiz. Que no exista no es un error.
func borrarEnJail(raiz, dir string) error {
	dir = filepath.Clean("/" + dir)
	if dir == "/" {
		return fmt.Errorf("refusing to remove the root of the jail %s", raiz)
	}
	padre, err := abrirDirSinEnlaces(raiz, filepath.Dir(dir), false)
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer padre.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(padre.Fd()), &st); err != nil {
		return err
	}
	return borrarSinCruzar(int(padre.Fd()), filepath.Base(dir), uint64(st.Dev), filepath.Join(raiz, dir))
}

// borrarSinCruzar borra la entrada nombre de dirfd, y si es un directorio,
// todo lo que tiene debajo. Como os.RemoveAll, no sigue enlaces (unlinkat
// borra el enlace, no su destino), pero además no entra en un directorio de
// otro sistema de ficheros que el de dev: un punto de montaje que siguiera
// ahí (el bind del almacén, ver borrarJail) haría que se vaciara lo montado,
// que no es del jail. Ruta es solo para los mensajes.
func borrarSinCruzar(dirfd int, nombre string, dev uint64, ruta string) error {
	err := jUnlinkat(dirfd, nombre, false)
	if err == nil || errors.Is(err, syscall.ENOENT) {
		return nil
	}
	// ¿Un directorio? Linux da EISDIR y macOS EPERM: se prueba a abrirlo como
	// tal, sin seguir un enlace. Si no lo es, el error es el del unlink.
	sub, oerr := jOpenat(dirfd, nombre, oDirSinEnlaces, 0)
	if oerr != nil {
		if errors.Is(oerr, syscall.ENOENT) {
			return nil
		}
		return fmt.Errorf("removing %s: %w", ruta, err)
	}
	d := os.NewFile(uintptr(sub), ruta)
	defer d.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(sub, &st); err != nil {
		return err
	}
	if uint64(st.Dev) != dev {
		return fmt.Errorf("%s is on another filesystem (a mount point?): not removing it", ruta)
	}
	// Varias pasadas, como os.RemoveAll: lo que se cree mientras se borra deja
	// el rmdir en ENOTEMPTY.
	for i := 0; ; i++ {
		nombres, err := d.Readdirnames(-1)
		if err != nil {
			return fmt.Errorf("listing %s: %w", ruta, err)
		}
		for _, n := range nombres {
			if err := borrarSinCruzar(sub, n, dev, filepath.Join(ruta, n)); err != nil {
				return err
			}
		}
		err = jUnlinkat(dirfd, nombre, true)
		if err == nil || errors.Is(err, syscall.ENOENT) {
			return nil
		}
		if !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) || i == 4 {
			return fmt.Errorf("removing %s: %w", ruta, err)
		}
		if _, err := d.Seek(0, 0); err != nil {
			return err
		}
	}
}
