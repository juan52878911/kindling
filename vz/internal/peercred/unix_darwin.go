//go:build darwin

package peercred

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// UID es el usuario efectivo del proceso al otro lado de una conexión por
// socket Unix, con getsockopt(SOL_LOCAL, LOCAL_PEERCRED): lo fija el kernel
// al conectar, y el cliente no puede falsearlo.
func UID(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return -1, errors.New("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return -1, err
	}
	var xu *unix.Xucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		xu, serr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return -1, err
	}
	if serr != nil {
		return -1, serr
	}
	if xu.Version != 0 { // XUCRED_VERSION
		return -1, fmt.Errorf("unexpected xucred version %d", xu.Version)
	}
	return int(xu.Uid), nil
}

// SameUser envuelve el listener del socket de la API (fc.sock) para que solo
// acepte conexiones de procesos del mismo usuario efectivo que este (el
// daemon que lo lanzó) o de root. Los permisos 0600 del socket no bastan: se ponen
// después del bind, y entre medias, con un directorio que deje entrar,
// cualquier usuario del Mac podía conectar y mandar a la VM (exec por el
// agente, snapshots, credenciales). Las demás conexiones se cierran sin
// leerles nada; logf (puede ser nil) lo cuenta como mucho una vez por
// minuto.
func SameUser(l net.Listener, logf func(string, ...any)) net.Listener {
	return &sameUser{Listener: l, uid: os.Geteuid(), logf: logf, peerUID: UID}
}

type sameUser struct {
	net.Listener
	uid     int
	logf    func(string, ...any)
	peerUID func(net.Conn) (int, error) // UID; los tests ponen otro

	mu        sync.Mutex
	rechazos  int
	ultimoLog time.Time
}

func (s *sameUser) Accept() (net.Conn, error) {
	for {
		c, err := s.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, err := s.peerUID(c)
		// root también: puede leer la memoria de este proceso de todos modos.
		if err == nil && (uid == s.uid || uid == 0) {
			return c, nil
		}
		_ = c.Close()
		s.avisar(uid, err)
	}
}

func (s *sameUser) avisar(uid int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rechazos++
	if s.logf == nil || time.Since(s.ultimoLog) < time.Minute {
		return
	}
	s.ultimoLog = time.Now()
	if err != nil {
		s.logf("API socket: rejected a connection whose peer could not be identified (%v); %d rejected so far", err, s.rechazos)
		return
	}
	s.logf("API socket: rejected a connection from uid %d (only uid %d or root may connect); %d rejected so far", uid, s.uid, s.rechazos)
}
