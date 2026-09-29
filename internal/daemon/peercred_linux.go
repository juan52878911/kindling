//go:build linux

package daemon

import (
	"errors"
	"net"
	"os"
	"syscall"
)

// credencialesPar lee quién está al otro lado del socket Unix (SO_PEERCRED):
// el uid y el gid efectivos del proceso que hizo connect(). Por SSH ese
// proceso es el `kling dial-stdio` que ssh lanza como el usuario remoto, así
// que el daemon ve al mismo usuario que se autenticó en SSH. Los grupos
// suplementarios no vienen en SO_PEERCRED: se resuelven por la base de
// usuarios cuando una regla de grupo los necesita (ver Politica.rol).
func credencialesPar(c net.Conn) (Llamante, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return Llamante{}, errors.New("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return Llamante{}, err
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return Llamante{}, err
	}
	if cerr != nil {
		return Llamante{}, cerr
	}
	return Llamante{UID: int(cred.Uid), GID: int(cred.Gid), Conocido: true}, nil
}

// dueñoFichero es el uid dueño de fi.
func dueñoFichero(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
