//go:build darwin

package machine

// Las llamadas *at que usa enjaulado.go, en macOS. Allí no hay jailer y nada
// de esto corre en producción; está para que los tests del jail corran
// también en el Mac. Por número, como openat y unlinkat en clon_darwin.go (el
// paquete syscall no las trae en darwin).

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

const (
	sysRenameat  = 465
	sysFstatat64 = 470
	sysMkdirat   = 475

	atRemoveDirDarwin     = 0x80
	atSymlinkNofollowDarw = 0x20
)

func jOpenat(dirfd int, nombre string, flags int, modo uint32) (int, error) {
	return openat(dirfd, nombre, flags, modo)
}

func jMkdirat(dirfd int, nombre string, modo uint32) error {
	p, err := syscall.BytePtrFromString(nombre)
	if err != nil {
		return err
	}
	_, _, e := syscall.Syscall(sysMkdirat, uintptr(dirfd), uintptr(unsafe.Pointer(p)), uintptr(modo))
	if e != 0 {
		return e
	}
	return nil
}

func jRenameat(viejo int, nombreViejo string, nuevo int, nombreNuevo string) error {
	a, err := syscall.BytePtrFromString(nombreViejo)
	if err != nil {
		return err
	}
	b, err := syscall.BytePtrFromString(nombreNuevo)
	if err != nil {
		return err
	}
	_, _, e := syscall.Syscall6(sysRenameat, uintptr(viejo), uintptr(unsafe.Pointer(a)),
		uintptr(nuevo), uintptr(unsafe.Pointer(b)), 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

func jUnlinkat(dirfd int, nombre string, dir bool) error {
	p, err := syscall.BytePtrFromString(nombre)
	if err != nil {
		return err
	}
	flags := 0
	if dir {
		flags = atRemoveDirDarwin
	}
	_, _, e := syscall.Syscall(sysUnlinkat, uintptr(dirfd), uintptr(unsafe.Pointer(p)), uintptr(flags))
	if e != 0 {
		return e
	}
	return nil
}

func jLstatat(dirfd int, nombre string, st *syscall.Stat_t) error {
	p, err := syscall.BytePtrFromString(nombre)
	if err != nil {
		return err
	}
	_, _, e := syscall.Syscall6(sysFstatat64, uintptr(dirfd), uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(st)), atSymlinkNofollowDarw, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

// rutaEnDir: en macOS no hay jail ni /proc; la ruta con la que se abrió.
func rutaEnDir(d *os.File, nombre string) string {
	return filepath.Join(d.Name(), nombre)
}
