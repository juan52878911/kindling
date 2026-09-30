//go:build linux

package machine

// Las llamadas *at que usa enjaulado.go, en Linux: casi todas están en el
// paquete syscall.

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// atRemoveDir es AT_REMOVEDIR de Linux.
const atRemoveDir = 0x200

func jOpenat(dirfd int, nombre string, flags int, modo uint32) (int, error) {
	return syscall.Openat(dirfd, nombre, flags, modo)
}

func jMkdirat(dirfd int, nombre string, modo uint32) error {
	return syscall.Mkdirat(dirfd, nombre, modo)
}

func jRenameat(viejo int, nombreViejo string, nuevo int, nombreNuevo string) error {
	return syscall.Renameat(viejo, nombreViejo, nuevo, nombreNuevo)
}

// jUnlinkat es unlinkat(2): nunca sigue un enlace, borra el propio enlace.
// Con dir, rmdir.
func jUnlinkat(dirfd int, nombre string, dir bool) error {
	p, err := syscall.BytePtrFromString(nombre)
	if err != nil {
		return err
	}
	flags := 0
	if dir {
		flags = atRemoveDir
	}
	_, _, e := syscall.Syscall(syscall.SYS_UNLINKAT, uintptr(dirfd), uintptr(unsafe.Pointer(p)), uintptr(flags))
	if e != 0 {
		return e
	}
	return nil
}

// jLstatat es fstatat(dirfd, nombre, AT_SYMLINK_NOFOLLOW) (lstatat, en
// put_seguro_linux.go): de un enlace da el enlace.
func jLstatat(dirfd int, nombre string, st *syscall.Stat_t) error {
	return lstatat(dirfd, nombre, st)
}

// rutaEnDir es una ruta que resuelve a nombre DENTRO del directorio abierto
// en d, pase lo que pase después con los componentes de su ruta: el enlace
// mágico de /proc apunta al inodo del descriptor, no a su nombre. Sirve para
// dárselo a quien solo sabe abrir por ruta (crearDestinoOverlay), que lo
// crea con O_EXCL|O_NOFOLLOW en el último componente.
func rutaEnDir(d *os.File, nombre string) string {
	return fmt.Sprintf("/proc/self/fd/%d/%s", d.Fd(), nombre)
}
