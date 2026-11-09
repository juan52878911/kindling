package fc

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// dialVMM conecta al socket de la API de un VMM sin seguir enlaces en el
// último componente y comprueba quién atiende (M3, ver par.go).
//
// Conectar por /proc/self/fd/N, además, deja la ruta del connect corta sea
// cual sea la del socket: en Linux no hace falta el enlace corto de dialUnix.
func dialVMM(ctx context.Context, path string) (net.Conn, error) {
	if !ComprobarJaula {
		return dialUnix(ctx, path)
	}
	fd, err := syscall.Open(path, syscall.O_PATH|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &net.OpError{Op: "dial", Net: "unix", Addr: &net.UnixAddr{Name: path, Net: "unix"}, Err: os.NewSyscallError("open", err)}
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("firecracker socket %s: %w", path, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFSOCK {
		// Un enlace simbólico (O_NOFOLLOW con O_PATH abre el enlace mismo) o
		// cualquier otra cosa que no es el socket que dejó el VMM.
		return nil, fmt.Errorf("firecracker socket %s is not a socket (a symlink or a planted file): refusing it", path)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", "/proc/self/fd/"+strconv.Itoa(fd))
	if err != nil {
		return nil, err
	}
	if !esSocketDeJaula(path) {
		return conn, nil
	}
	if err := comprobarParDe(conn, path); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// comprobarParDe lee el pid del otro lado de conn (SO_PEERCRED) y compara su
// raíz con la jaula de path.
func comprobarParDe(conn net.Conn, path string) error {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("firecracker socket %s: not a unix connection", path)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var cred *syscall.Ucred
	var errCred error
	if err := raw.Control(func(fd uintptr) {
		cred, errCred = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if errCred != nil {
		return fmt.Errorf("firecracker socket %s: reading its peer: %w", path, errCred)
	}
	raizPar, err := idDe("/proc/" + strconv.Itoa(int(cred.Pid)) + "/root")
	if err != nil {
		// Sin poder mirar su raíz (un daemon sin root no puede con un VMM de
		// otro usuario) no hay jailer posible: jailer exige root. Se rechaza
		// igual, porque la ruta dice que es de una jaula.
		return fmt.Errorf("firecracker socket %s: checking the root of its peer (pid %d): %w", path, cred.Pid, err)
	}
	raizHost, err := idDe("/proc/self/root")
	if err != nil {
		return err
	}
	jaula, errJaula := idDe(filepath.Dir(filepath.Dir(path)))
	return comprobarPar(path, raizPar, raizHost, jaula, errJaula)
}

// idDe es el dispositivo e inodo de path, siguiendo enlaces (también el
// mágico de /proc/<pid>/root, que lleva a la raíz de ese proceso).
func idDe(path string) (idFichero, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return idFichero{}, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	return idFichero{dev: uint64(st.Dev), ino: st.Ino}, nil
}
