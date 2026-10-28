//go:build linux

package guest

import (
	"errors"
	"net"
	"os"
	"syscall"
	"unsafe"
)

// sockaddrVM es struct sockaddr_vm (linux/vm_sockets.h). El paquete syscall
// no la trae y el módulo raíz no depende de x/sys: son 16 bytes.
type sockaddrVM struct {
	family    uint16
	reserved1 uint16
	port      uint32
	cid       uint32
	flags     uint8
	zero      [3]uint8
}

// vmaddrCIDAny: escuchar en cualquier CID (VMADDR_CID_ANY).
const vmaddrCIDAny = 0xFFFFFFFF

// afVsock es AF_VSOCK (40), que el paquete syscall no define en todas las
// arquitecturas ni en las versiones viejas de Go.
const afVsock = 40

// ListenVsock escucha conexiones AF_VSOCK de tipo stream en port.
//
// net.FileListener no sirve: el paquete net no conoce la familia y lo
// rechaza. El descriptor va no bloqueante dentro de un *os.File, que lo
// apunta al netpoller: Accept espera sin ocupar un hilo, y Close lo despierta.
func ListenVsock(port uint32) (net.Listener, error) {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, os.NewSyscallError("socket", err)
	}
	sa := sockaddrVM{family: afVsock, port: port, cid: vmaddrCIDAny}
	if _, _, e := syscall.Syscall(syscall.SYS_BIND, uintptr(fd), uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa)); e != 0 {
		syscall.Close(fd)
		return nil, os.NewSyscallError("bind", e)
	}
	if err := syscall.Listen(fd, syscall.SOMAXCONN); err != nil {
		syscall.Close(fd)
		return nil, os.NewSyscallError("listen", err)
	}
	return &vsockListener{f: os.NewFile(uintptr(fd), "vsock-listener"), addr: vsockAddr{cid: vmaddrCIDAny, port: port}}, nil
}

type vsockListener struct {
	f    *os.File
	addr vsockAddr
}

func (l *vsockListener) Accept() (net.Conn, error) {
	rc, err := l.f.SyscallConn()
	if err != nil {
		return nil, err
	}
	var nfd int
	var aerr error
	err = rc.Read(func(fd uintptr) bool {
		// Sin dirección de vuelta: syscall.Accept4 la traduciría y, con
		// una familia que no conoce, cerraría la conexión y daría error.
		r, _, e := syscall.Syscall6(syscall.SYS_ACCEPT4, fd, 0, 0, syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0, 0)
		switch e {
		case 0:
			nfd, aerr = int(r), nil
		case syscall.EAGAIN:
			return false
		default:
			aerr = os.NewSyscallError("accept4", e)
		}
		return true
	})
	if err != nil {
		if errors.Is(err, os.ErrClosed) {
			return nil, net.ErrClosed
		}
		return nil, err
	}
	if aerr != nil {
		return nil, aerr
	}
	c := newVsockConn(os.NewFile(uintptr(nfd), "vsock"), l.addr)
	// Quién llama: requestFromGuest solo deja conectar carpetas al host.
	var sa sockaddrVM
	n := uint32(unsafe.Sizeof(sa))
	if _, _, e := syscall.Syscall(syscall.SYS_GETPEERNAME, uintptr(nfd), uintptr(unsafe.Pointer(&sa)), uintptr(unsafe.Pointer(&n))); e == 0 {
		c.remote = vsockAddr{cid: sa.cid, port: sa.port}
	} else {
		c.remote = vsockAddr{}
	}
	return c, nil
}

func (l *vsockListener) Close() error   { return l.f.Close() }
func (l *vsockListener) Addr() net.Addr { return l.addr }
