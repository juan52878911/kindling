//go:build darwin

package machine

// clonefile desde un descriptor en macOS (commit, ver copiarOverlayDesde).
//
// `cp -c` clona por ruta, y la ruta del overlay es del VMM: el commit copia
// desde el descriptor ya comprobado (fijarOverlayParaLeer), y `cp -c
// /dev/fd/N` no clona. fclonefileat(2) sí clona desde un descriptor. No está
// en el paquete syscall ni hay cgo en el núcleo, así que se llama por su
// número, como renameat y readlinkat en internal/share: syscall.Syscall6 en
// darwin pasa por syscall(2) de libc, que acepta cualquier número. Los
// números son de XNU (bsd/kern/syscalls.master, y SYS_* en <sys/syscall.h>) y
// estables desde macOS 10.12.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

const (
	sysOpenat       = 463
	sysUnlinkat     = 472
	sysFclonefileat = 517

	// CLONE_NOOWNERCOPY: el clon es de quien lo crea, no del dueño del
	// origen (el VMM). Sin él, un daemon root dejaría el dorado a nombre del
	// VMM hasta cederlo.
	cloneNoOwnerCopy = 0x0002
)

// errNoClona dice que fclonefileat no pudo clonar (ENOTSUP fuera de APFS,
// EXDEV entre volúmenes, EEXIST...): se copia como siempre, que da el mismo
// error si lo hay de verdad.
var errNoClona = errors.New("fclonefileat did not clone")

// fclonefileat es la llamada al sistema; variable para que un test simule
// ENOTSUP o EXDEV en un disco APFS.
var fclonefileat = func(src, dirfd int, nombre string, flags int) error {
	p, err := syscall.BytePtrFromString(nombre)
	if err != nil {
		return err
	}
	_, _, e := syscall.Syscall6(sysFclonefileat, uintptr(src), uintptr(dirfd),
		uintptr(unsafe.Pointer(p)), uintptr(flags), 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

func openat(dirfd int, nombre string, flags int, modo uint32) (int, error) {
	p, err := syscall.BytePtrFromString(nombre)
	if err != nil {
		return -1, err
	}
	fd, _, e := syscall.Syscall6(sysOpenat, uintptr(dirfd), uintptr(unsafe.Pointer(p)),
		uintptr(flags), uintptr(modo), 0, 0)
	if e != 0 {
		return -1, e
	}
	return int(fd), nil
}

func unlinkat(dirfd int, nombre string) {
	p, err := syscall.BytePtrFromString(nombre)
	if err != nil {
		return
	}
	_, _, _ = syscall.Syscall6(sysUnlinkat, uintptr(dirfd), uintptr(unsafe.Pointer(p)), 0, 0, 0, 0)
}

// crearDestinoOverlay crea dst para el overlay abierto en in y dice si ya lo
// tiene (clonado) o hay que copiarlo. En macOS clona con fclonefileat desde
// el descriptor; si no se puede, lo crea vacío con O_EXCL|O_NOFOLLOW.
func crearDestinoOverlay(in *os.File, dst string, _ bool) (*os.File, bool, error) {
	out, err := clonarADestino(in, dst)
	if err == nil {
		return out, true, nil
	}
	if !errors.Is(err, errNoClona) {
		return nil, false, err
	}
	out, err = os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	return out, false, err
}

// clonarADestino clona in en dst con fclonefileat y devuelve dst abierto.
//
// fclonefileat crea el fichero él mismo y falla con EEXIST si ya hay algo con
// ese nombre, enlace incluido: nunca escribe a través de un enlace plantado.
// Pero entre crearlo y abrirlo el nombre podría cambiar de fichero (el
// directorio puede ser del VMM), así que se abre relativo al mismo directorio,
// sin seguir enlaces, y se exige lo que solo cumple el clon recién hecho:
// fichero regular, un solo enlace (no un hardlink a otro fichero), de quien
// clona (CLONE_NOOWNERCOPY) y del tamaño del origen. El llamante comprueba
// además, al recuperarlo, que es este mismo inodo.
func clonarADestino(in *os.File, dst string) (*os.File, error) {
	dir, nombre := filepath.Dir(dst), filepath.Base(dst)
	dfd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(dfd)
	if err := fclonefileat(int(in.Fd()), dfd, nombre, cloneNoOwnerCopy); err != nil {
		return nil, fmt.Errorf("%w: %v", errNoClona, err)
	}
	fd, err := openat(dfd, nombre, syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		unlinkat(dfd, nombre)
		return nil, fmt.Errorf("opening the clone %s: %w", dst, err)
	}
	out := os.NewFile(uintptr(fd), dst)
	if err := esClonDe(in, out); err != nil {
		out.Close()
		unlinkat(dfd, nombre)
		return nil, fmt.Errorf("the clone %s: %w", dst, err)
	}
	// El clon hereda el modo del origen: se deja como un fichero creado aquí.
	if err := out.Chmod(0o600); err != nil {
		out.Close()
		unlinkat(dfd, nombre)
		return nil, err
	}
	return out, nil
}

// esClonDe comprueba que out es el fichero que acaba de crear fclonefileat
// desde in (ver clonarADestino).
func esClonDe(in, out *os.File) error {
	var so, sd syscall.Stat_t
	if err := syscall.Fstat(int(in.Fd()), &so); err != nil {
		return err
	}
	if err := syscall.Fstat(int(out.Fd()), &sd); err != nil {
		return err
	}
	switch {
	case sd.Mode&syscall.S_IFMT != syscall.S_IFREG:
		return errors.New("not a regular file")
	case sd.Nlink != 1:
		return fmt.Errorf("it has %d links", sd.Nlink)
	case int(sd.Uid) != os.Geteuid():
		return fmt.Errorf("it belongs to uid %d", sd.Uid)
	case sd.Size != so.Size:
		return fmt.Errorf("it has %d bytes and the overlay %d", sd.Size, so.Size)
	}
	return nil
}
