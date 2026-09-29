//go:build linux

package machine

// ESCRIBIR DENTRO DE UNA IMAGEN MONTADA SIN SALIRSE DE ELLA.
//
// intentarPut monta la imagen en mnt y antes escribía en filepath.Join(mnt,
// ruta) con las llamadas de siempre. Pero los enlaces simbólicos de la imagen
// los resuelve el kernel contra la raíz del HOST: con /usr/local -> /etc en la
// imagen, poner /usr/local/x escribía /etc/x del host, como root. Y una imagen
// no es de fiar: la trae quien la construye o la copia.
//
// Así que la ruta se resuelve a mano, como en macOS (put_debugfs.go):
//
//   - componente a componente desde un descriptor de mnt, con openat y
//     O_NOFOLLOW relativo al directorio anterior: el kernel nunca sigue un
//     enlace por su cuenta;
//   - un enlace de un directorio intermedio se lee (readlinkat) y se sigue
//     DENTRO de la imagen: un destino absoluto desde su raíz, un ".." que no pasa
//     de ella (path.Clean), y como mucho maxEnlacesRuta saltos;
//   - lo que se crea, se lee o se renombra va relativo al descriptor del
//     directorio final (mkdirat, openat, renameat), así que no hay carrera
//     entre resolver y escribir: si alguien cambia un componente por un enlace
//     entre medias, O_NOFOLLOW falla en vez de seguirlo.
//
// El último componente no se sigue: si es un enlace, se reemplaza el enlace
// (como hacía el rename de siempre), sin leer ni escribir su destino.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"syscall"
	"unsafe"
)

const (
	oDir  = syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	oLeer = syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
)

func readlinkat(dirfd int, name string) (string, error) {
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return "", err
	}
	buf := make([]byte, 4096)
	n, _, e := syscall.Syscall6(syscall.SYS_READLINKAT, uintptr(dirfd), uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)
	if e != 0 {
		return "", e
	}
	return string(buf[:n]), nil
}

// dirEnImagen es el directorio final de una ruta dentro de la imagen.
type dirEnImagen struct {
	fd   int
	ruta string // la ruta dentro de la imagen, ya sin enlaces
}

func (d dirEnImagen) Close() { syscall.Close(d.fd) }

// abrirDirEnImagen resuelve dir dentro de la imagen montada en mnt y abre su
// último directorio. Con crear, crea los que falten (0755); sin crear y si
// falta alguno, devuelve os.ErrNotExist.
func abrirDirEnImagen(mnt, dir string, crear bool) (dirEnImagen, error) {
	raiz, err := syscall.Open(mnt, oDir, 0)
	if err != nil {
		return dirEnImagen{}, fmt.Errorf("opening %s: %w", mnt, err)
	}
	defer syscall.Close(raiz)

	cur, err := syscall.Dup(raiz)
	if err != nil {
		return dirEnImagen{}, err
	}
	ruta := "/"
	pendientes := partes(dir)
	for saltos := 0; len(pendientes) > 0; {
		comp := pendientes[0]
		var st syscall.Stat_t
		err := lstatat(cur, comp, &st)
		switch {
		case errors.Is(err, syscall.ENOENT):
			if !crear {
				syscall.Close(cur)
				return dirEnImagen{}, os.ErrNotExist
			}
			if err := syscall.Mkdirat(cur, comp, 0o755); err != nil && !errors.Is(err, syscall.EEXIST) {
				syscall.Close(cur)
				return dirEnImagen{}, fmt.Errorf("creating %s: %w", path.Join(ruta, comp), err)
			}
			continue // se vuelve a mirar: ahora es un directorio (o lo que alguien puso)
		case err != nil:
			syscall.Close(cur)
			return dirEnImagen{}, fmt.Errorf("%s: %w", path.Join(ruta, comp), err)
		case st.Mode&syscall.S_IFMT == syscall.S_IFLNK:
			if saltos++; saltos > maxEnlacesRuta {
				syscall.Close(cur)
				return dirEnImagen{}, fmt.Errorf("too many symlinks resolving %s in the image", dir)
			}
			destino, err := readlinkat(cur, comp)
			syscall.Close(cur)
			if err != nil {
				return dirEnImagen{}, fmt.Errorf("reading the symlink %s: %w", path.Join(ruta, comp), err)
			}
			base := ruta
			if path.IsAbs(destino) {
				base = "/" // desde la raíz de la IMAGEN, nunca la del host
			}
			// Clean no deja que un ".." pase de la raíz.
			nuevo := path.Clean(path.Join(append([]string{base, destino}, pendientes[1:]...)...))
			if cur, err = syscall.Dup(raiz); err != nil {
				return dirEnImagen{}, err
			}
			ruta, pendientes = "/", partes(nuevo)
		case st.Mode&syscall.S_IFMT == syscall.S_IFDIR:
			sig, err := syscall.Openat(cur, comp, oDir, 0)
			syscall.Close(cur)
			if err != nil {
				// ELOOP/ENOTDIR: lo cambiaron por un enlace entre medias.
				return dirEnImagen{}, fmt.Errorf("opening %s: %w", path.Join(ruta, comp), err)
			}
			cur, ruta, pendientes = sig, path.Join(ruta, comp), pendientes[1:]
		default:
			syscall.Close(cur)
			return dirEnImagen{}, fmt.Errorf("%s is not a directory in the image", path.Join(ruta, comp))
		}
	}
	return dirEnImagen{fd: cur, ruta: ruta}, nil
}

// oPath es O_PATH (igual en amd64 y arm64), que el paquete syscall no exporta.
const oPath = 0x200000

// lstatat es fstatat(dirfd, name, AT_SYMLINK_NOFOLLOW), que syscall no da en
// todas las arquitecturas: un descriptor O_PATH|O_NOFOLLOW del propio nombre
// (el enlace, si lo es) y fstat.
func lstatat(dirfd int, name string, st *syscall.Stat_t) error {
	fd, err := syscall.Openat(dirfd, name, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	return syscall.Fstat(fd, st)
}

// sha256EnDir es el digest de name en d, sin seguir un enlace. "" si no es un
// fichero normal.
func sha256EnDir(d dirEnImagen, name string) (string, error) {
	fd, err := syscall.Openat(d.fd, name, oLeer, 0)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ponerEnImagen es la parte de intentarPut que toca la imagen montada: mismo
// contrato (cambió, errNoBridge, errSinHueco), sin salir nunca de mnt.
func ponerEnImagen(mnt, dentroPath, src, quiero string, mode os.FileMode, create bool) (bool, error) {
	limpio := path.Clean("/" + dentroPath)
	if limpio == "/" {
		return false, fmt.Errorf("invalid path %q", dentroPath)
	}
	dir, nombre := path.Split(limpio)
	d, err := abrirDirEnImagen(mnt, dir, create)
	if errors.Is(err, os.ErrNotExist) {
		return false, errNoBridge
	}
	if err != nil {
		return false, err
	}
	defer d.Close()
	dentro := path.Join(d.ruta, nombre)

	var st syscall.Stat_t
	err = lstatat(d.fd, nombre, &st)
	existe := err == nil
	switch {
	case err != nil && !errors.Is(err, syscall.ENOENT):
		return false, fmt.Errorf("%s: %w", dentro, err)
	case !existe && !create:
		return false, errNoBridge
	case existe && st.Mode&syscall.S_IFMT == syscall.S_IFDIR:
		return false, fmt.Errorf("%s is a directory in the image, not a file", dentro)
	case existe && st.Mode&syscall.S_IFMT == syscall.S_IFREG:
		tengo, err := sha256EnDir(d, nombre)
		if err != nil {
			return false, fmt.Errorf("reading %s: %w", dentro, err)
		}
		if tengo == quiero {
			return false, nil
		}
	}

	in, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return false, err
	}
	if !fi.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", src)
	}
	// ¿Cabe al lado del viejo? Ver faltaParaElPuente.
	var sfs syscall.Statfs_t
	if err := syscall.Fstatfs(d.fd, &sfs); err == nil {
		if falta := faltaParaElPuente(int64(sfs.Bavail)*int64(sfs.Bsize), fi.Size()); falta > 0 {
			return false, errSinHueco{faltan: falta}
		}
	}

	// Al lado y renombrado: atómico, como siempre. El .nuevo de un intento
	// anterior se quita sin seguirlo (unlinkat no sigue enlaces).
	tmp := nombre + ".nuevo"
	_ = syscall.Unlinkat(d.fd, tmp)
	fd, err := syscall.Openat(d.fd, tmp, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return false, fmt.Errorf("creating %s: %w", path.Join(d.ruta, tmp), err)
	}
	out := os.NewFile(uintptr(fd), tmp)
	quitar := func() { _ = syscall.Unlinkat(d.fd, tmp) }
	_, err = io.Copy(out, in)
	if err == nil {
		// El modo exacto, sin la umask del daemon.
		err = out.Chmod(mode.Perm())
	}
	if err == nil {
		// Sync antes del renombrado: éste es atómico en los metadatos, no en
		// los datos.
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		quitar()
		return false, fmt.Errorf("copying %s: %w", dentro, err)
	}
	if err := syscall.Renameat(d.fd, tmp, d.fd, nombre); err != nil {
		quitar()
		return false, fmt.Errorf("replacing %s: %w", dentro, err)
	}
	return true, nil
}
