//go:build darwin

package daemon

import (
	"errors"
	"net"
	"os"
	"syscall"
	"unsafe"
)

// xucred es struct xucred de <sys/ucred.h>: versión, uid efectivo y hasta
// XU_NGROUPS (16) grupos, el primero el efectivo.
type xucred struct {
	Version uint32
	UID     uint32
	NGroups int16
	_       int16
	Groups  [16]uint32
}

const (
	solLocal      = 0 // SOL_LOCAL
	localPeercred = 1 // LOCAL_PEERCRED
	xucredVersion = 0 // XUCRED_VERSION
)

// credencialesPar lee quién está al otro lado del socket Unix con
// getsockopt(SOL_LOCAL, LOCAL_PEERCRED). El paquete syscall no lo envuelve y
// el núcleo no usa cgo ni x/sys, así que va por Syscall6 (lo que hace x/sys
// por dentro). Da el uid efectivo y los grupos del proceso, como mucho 16: un
// usuario con más grupos puede no casar con una regla de grupo; las reglas de
// uid o de usuario no tienen ese límite.
func credencialesPar(c net.Conn) (Llamante, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return Llamante{}, errors.New("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return Llamante{}, err
	}
	var xu xucred
	var errno syscall.Errno
	if err := raw.Control(func(fd uintptr) {
		l := uint32(unsafe.Sizeof(xu))
		_, _, errno = syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, solLocal, localPeercred,
			uintptr(unsafe.Pointer(&xu)), uintptr(unsafe.Pointer(&l)), 0)
	}); err != nil {
		return Llamante{}, err
	}
	if errno != 0 {
		return Llamante{}, errno
	}
	if xu.Version != xucredVersion {
		return Llamante{}, errors.New("unexpected xucred version")
	}
	n := int(xu.NGroups)
	if n < 0 || n > len(xu.Groups) {
		n = 0
	}
	ll := Llamante{UID: int(xu.UID), Conocido: true, Groups: []int{}}
	for _, g := range xu.Groups[:n] {
		ll.Groups = append(ll.Groups, int(g))
	}
	if n > 0 {
		ll.GID = int(xu.Groups[0])
	}
	return ll, nil
}

// dueñoFichero es el uid dueño de fi.
func dueñoFichero(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
