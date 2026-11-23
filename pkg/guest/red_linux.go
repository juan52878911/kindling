//go:build linux

package guest

import (
	"encoding/binary"
	"errors"
	"net"
	"syscall"
	"time"
)

// configurarRedOS manda los mensajes de configurarRed por un solo socket
// NETLINK_ROUTE. eth0 la crea el driver virtio antes del init, pero se
// reintenta unos ms por si aparece tarde.
func configurarRedOS(v string) error {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	tv := syscall.NsecToTimeval(int64(time.Second))
	_ = syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return err
	}
	indice := func(dev string) (int32, error) {
		var err error
		for range 200 {
			var ifi *net.Interface
			if ifi, err = net.InterfaceByName(dev); err == nil {
				return int32(ifi.Index), nil
			}
			time.Sleep(5 * time.Millisecond)
		}
		return 0, err
	}
	emisor := func(_ string, msg []byte) error {
		seq := binary.NativeEndian.Uint32(msg[8:])
		if err := syscall.Sendto(fd, msg, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
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
				return errors.New("netlink: empty answer")
			}
		}
	}
	return configurarRed(v, emisor, indice)
}
