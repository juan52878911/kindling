//go:build darwin

package peercred

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

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

// PID es el proceso al otro lado de una conexión por socket Unix
// (LOCAL_PEERPID, que también fija el kernel al conectar).
func PID(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return -1, errors.New("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return -1, err
	}
	var pid int
	var serr error
	if err := raw.Control(func(fd uintptr) {
		pid, serr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return -1, err
	}
	if serr != nil {
		return -1, serr
	}
	return pid, nil
}

// Ejecutable es la ruta del ejecutable del proceso pid (proc_pidpath).
func Ejecutable(pid int) (string, error) {
	buf := make([]byte, 4*1024) // PROC_PIDPATHINFO_MAXSIZE
	// proc_info(PROC_INFO_CALL_PIDINFO, pid, PROC_PIDPATHINFO, 0, buf, len):
	// lo que hace proc_pidpath de libproc; la ruta acaba en NUL.
	_, _, e := unix.Syscall6(unix.SYS_PROC_INFO, 2, uintptr(pid), 11, 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if e != 0 {
		return "", fmt.Errorf("proc_pidpath: %w", e)
	}
	n := bytes.IndexByte(buf, 0)
	if n <= 0 {
		return "", errors.New("proc_pidpath returned nothing")
	}
	return string(buf[:n]), nil
}

// MismoEjecutable dice si ruta (la de Ejecutable) es bin, tras resolver los
// enlaces de bin.
func MismoEjecutable(ruta, bin string) bool {
	if ruta == "" || bin == "" || !filepath.IsAbs(bin) {
		return false
	}
	if r, err := filepath.EvalSymlinks(bin); err == nil {
		bin = r
	}
	return filepath.Clean(ruta) == filepath.Clean(bin)
}

// Solo envuelve un listener de socket Unix para que acepte únicamente
// conexiones de este mismo usuario (o root) cuyo ejecutable sea uno de bins:
// el API de kling-credproxy (solo el daemon) y su reenvío (solo el VMM de su
// máquina). Las demás se cierran sin leerles nada; lo cuenta por logf como
// mucho una vez por minuto.
func Solo(l net.Listener, que string, bins []string, logf func(string, ...any)) net.Listener {
	return &solo{Listener: l, que: que, bins: bins, uid: os.Geteuid(), logf: logf}
}

// SoloPID es Solo exigiendo además que al otro lado esté el proceso pid (el
// reenvío de kling-credproxy: el VMM de su máquina y ningún otro).
func SoloPID(l net.Listener, que string, bins []string, pid int, logf func(string, ...any)) net.Listener {
	return &solo{Listener: l, que: que, bins: bins, pid: pid, uid: os.Geteuid(), logf: logf}
}

type solo struct {
	net.Listener
	que  string
	bins []string
	pid  int // 0: cualquiera
	uid  int
	logf func(string, ...any)

	mu        sync.Mutex
	rechazos  int
	ultimoLog time.Time
}

// permitido dice por qué no se admite c, o nil.
func (s *solo) permitido(c net.Conn) error {
	uid, err := UID(c)
	if err != nil {
		return err
	}
	if uid != s.uid && uid != 0 {
		return fmt.Errorf("uid %d", uid)
	}
	pid, err := PID(c)
	if err != nil {
		return err
	}
	if s.pid != 0 && pid != s.pid {
		return fmt.Errorf("pid %d is not %d", pid, s.pid)
	}
	ruta, err := Ejecutable(pid)
	if err != nil {
		return err
	}
	for _, b := range s.bins {
		if MismoEjecutable(ruta, b) {
			return nil
		}
	}
	return fmt.Errorf("pid %d runs %s", pid, ruta)
}

func (s *solo) Accept() (net.Conn, error) {
	for {
		c, err := s.Listener.Accept()
		if err != nil {
			return nil, err
		}
		perr := s.permitido(c)
		if perr == nil {
			return c, nil
		}
		_ = c.Close()
		if errors.Is(perr, unix.ENOTCONN) {
			continue // ya se fue (un sondeo de si escucha): nada que contar
		}
		s.mu.Lock()
		s.rechazos++
		if s.logf != nil && time.Since(s.ultimoLog) >= time.Minute {
			s.ultimoLog = time.Now()
			s.logf("%s: rejected a connection (%v); %d rejected so far", s.que, perr, s.rechazos)
		}
		s.mu.Unlock()
	}
}
