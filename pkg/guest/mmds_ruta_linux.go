//go:build linux

package guest

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
)

// rutaNetlinkOS manda el RTM_NEWROUTE de mensajeRuta por un socket
// NETLINK_ROUTE y espera su ACK. EEXIST llega como syscall.EEXIST.
func rutaNetlinkOS(dst [4]byte, dev string) error {
	ifi, err := net.InterfaceByName(dev)
	if err != nil {
		return err
	}
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	// Un plazo: el kernel contesta en microsegundos, pero un PID 1 colgado
	// aquí no escucharía nunca.
	tv := syscall.NsecToTimeval(int64(time.Second))
	_ = syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return err
	}
	const seq = 1
	if err := syscall.Sendto(fd, mensajeRuta(seq, dst, int32(ifi.Index)), 0,
		&syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if errno, ok := errnoRespuesta(buf[:n], seq); ok {
			if errno == 0 {
				return nil
			}
			return syscall.Errno(errno)
		}
		if n == 0 {
			return fmt.Errorf("netlink: empty answer")
		}
	}
}
