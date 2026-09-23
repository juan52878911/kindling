//go:build linux

package share

import (
	"os"
	"syscall"
	"time"
	"unsafe"

	proto "github.com/juan52878911/kindling/pkg/share"
)

func statTimes(st *syscall.Stat_t) (atime, ctime time.Time) {
	return time.Unix(st.Atim.Sec, st.Atim.Nsec), time.Unix(st.Ctim.Sec, st.Ctim.Nsec)
}

// renameat renombra entre dos directorios abiertos por os.Root. Sobre
// descriptores y nombres sueltos: ninguno de los dos últimos componentes se
// sigue si es un enlace.
func renameat(oldDir *os.File, oldName string, newDir *os.File, newName string) error {
	return syscall.Renameat(int(oldDir.Fd()), oldName, int(newDir.Fd()), newName)
}

func readlinkat(dir *os.File, name string) (string, error) {
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return "", err
	}
	buf := make([]byte, proto.MaxPath)
	n, _, e := syscall.Syscall6(syscall.SYS_READLINKAT, dir.Fd(), uintptr(unsafe.Pointer(p)),
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
		Files: st.Files, Ffree: st.Ffree, Bsize: uint32(st.Bsize), Namelen: uint32(st.Namelen)}, nil
}
