//go:build darwin

package share

import (
	"os"
	"syscall"
	"time"
	"unsafe"

	proto "github.com/juan52878911/kindling/pkg/share"
)

// Números de llamada de XNU (bsd/kern/syscalls.master). El paquete syscall de
// Go no trae renameat ni readlinkat para darwin y os.Root no los tiene hasta Go
// 1.25; el núcleo se compila con 1.24 y sin dependencias, así que se llaman
// directamente. Son estables desde macOS 10.10.
const (
	sysRenameat   = 465
	sysReadlinkat = 473
)

func statTimes(st *syscall.Stat_t) (atime, ctime time.Time) {
	return time.Unix(st.Atimespec.Sec, st.Atimespec.Nsec), time.Unix(st.Ctimespec.Sec, st.Ctimespec.Nsec)
}

func renameat(oldDir *os.File, oldName string, newDir *os.File, newName string) error {
	a, err := syscall.BytePtrFromString(oldName)
	if err != nil {
		return err
	}
	b, err := syscall.BytePtrFromString(newName)
	if err != nil {
		return err
	}
	_, _, e := syscall.Syscall6(sysRenameat, oldDir.Fd(), uintptr(unsafe.Pointer(a)),
		newDir.Fd(), uintptr(unsafe.Pointer(b)), 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

func readlinkat(dir *os.File, name string) (string, error) {
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return "", err
	}
	buf := make([]byte, proto.MaxPath)
	n, _, e := syscall.Syscall6(sysReadlinkat, dir.Fd(), uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)
	if e != 0 {
		return "", e
	}
	return string(buf[:n]), nil
}

func fstatfs(f *os.File) (proto.Statfs, error) {
	var st syscall.Statfs_t
	if err := syscall.Fstatfs(int(f.Fd()), &st); err != nil {
		return proto.Statfs{}, err
	}
	return proto.Statfs{Blocks: st.Blocks, Bfree: st.Bfree, Bavail: st.Bavail,
		Files: st.Files, Ffree: st.Ffree, Bsize: st.Bsize, Namelen: 255}, nil
}
