//go:build linux

package net

import (
	"errors"
	"fmt"
	stdnet "net"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"
)

// conexionIPSet es un socket NETLINK_NETFILTER abierto DENTRO de un netns:
// un socket se queda en el netns en el que se creó, así que basta con
// crearlo una vez allí y usarlo desde cualquier hilo.
type conexionIPSet struct {
	mu       sync.Mutex
	fd       int
	seq      uint32
	dev, ino uint64 // del /run/netns/<ns> en el que se abrió
}

var (
	conexionesMu sync.Mutex
	conexiones   = map[string]*conexionIPSet{}
)

func rutaNetns(ns string) string { return "/var/run/netns/" + ns }

// identidadNetns es el dispositivo e inodo del netns ns: si el netns se borra
// y se crea otro con el mismo nombre, cambian, y un socket del viejo metería
// las IP en un netns que ya no es el de la máquina.
func identidadNetns(ns string) (uint64, uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(rutaNetns(ns), &st); err != nil {
		return 0, 0, err
	}
	return uint64(st.Dev), uint64(st.Ino), nil
}

func anadirIPSetNetlink(ns, set, ip string, timeout int) error {
	v4 := stdnet.ParseIP(ip).To4()
	if v4 == nil {
		return fmt.Errorf("ipset netlink: %q is not IPv4", ip)
	}
	c, err := conexionDe(ns)
	if err != nil {
		return err
	}
	if err := c.anadir(set, [4]byte(v4), timeout); err != nil {
		var rechazo *rechazoIPSet
		if !errors.As(err, &rechazo) {
			cerrarIPSet(ns) // la conexión falló: la siguiente vuelve a abrir
		}
		return err
	}
	return nil
}

// conexionDe da la conexión del netns ns, abriéndola si no la hay o si el
// netns ya no es el mismo.
func conexionDe(ns string) (*conexionIPSet, error) {
	dev, ino, err := identidadNetns(ns)
	if err != nil {
		return nil, err
	}
	conexionesMu.Lock()
	defer conexionesMu.Unlock()
	if c := conexiones[ns]; c != nil {
		if c.dev == dev && c.ino == ino {
			return c, nil
		}
		c.cerrar()
		delete(conexiones, ns)
	}
	fd, err := socketEnNetns(rutaNetns(ns))
	if err != nil {
		return nil, err
	}
	c := &conexionIPSet{fd: fd, dev: dev, ino: ino}
	conexiones[ns] = c
	return c, nil
}

// cerrarIPSet suelta la conexión del netns ns, si la hay. Teardown la llama
// al borrar el netns.
func cerrarIPSet(ns string) {
	conexionesMu.Lock()
	c := conexiones[ns]
	delete(conexiones, ns)
	conexionesMu.Unlock()
	if c != nil {
		c.cerrar()
	}
}

func (c *conexionIPSet) cerrar() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fd >= 0 {
		_ = syscall.Close(c.fd)
		c.fd = -1
	}
}

// socketEnNetns crea el socket netlink dentro del netns de ruta. Se hace en
// una goroutine propia con su hilo bloqueado: setns cambia el netns del HILO.
// Si no se puede volver al netns original, el hilo no se suelta y muere con
// la goroutine, para que ningún otro código del daemon corra en el netns de
// un invitado.
func socketEnNetns(ruta string) (int, error) {
	type res struct {
		fd  int
		err error
	}
	ch := make(chan res, 1)
	go func() {
		runtime.LockOSThread()
		volvio := false
		defer func() {
			if volvio {
				runtime.UnlockOSThread()
			}
		}()
		orig, err := os.Open("/proc/thread-self/ns/net")
		if err != nil {
			volvio = true
			ch <- res{-1, err}
			return
		}
		defer orig.Close()
		dest, err := os.Open(ruta)
		if err != nil {
			volvio = true
			ch <- res{-1, err}
			return
		}
		defer dest.Close()
		if err := setns(int(dest.Fd())); err != nil {
			volvio = true
			ch <- res{-1, fmt.Errorf("setns %s: %w", ruta, err)}
			return
		}
		fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_NETFILTER)
		volvio = setns(int(orig.Fd())) == nil
		if err == nil && !volvio {
			_ = syscall.Close(fd)
			err = errors.New("setns back to the host netns failed")
		}
		if err != nil {
			ch <- res{-1, err}
			return
		}
		// Un plazo: el kernel contesta en microsegundos, pero el resolver no
		// puede quedarse colgado aquí.
		tv := syscall.NsecToTimeval(int64(time.Second))
		_ = syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
		if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
			_ = syscall.Close(fd)
			ch <- res{-1, err}
			return
		}
		ch <- res{fd, nil}
	}()
	r := <-ch
	return r.fd, r.err
}

func setns(fd int) error {
	if sysSetns < 0 {
		return syscall.ENOSYS
	}
	if _, _, e := syscall.RawSyscall(uintptr(sysSetns), uintptr(fd), syscall.CLONE_NEWNET, 0); e != 0 {
		return e
	}
	return nil
}

func (c *conexionIPSet) anadir(set string, ip [4]byte, timeout int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fd < 0 {
		return errors.New("ipset netlink: closed")
	}
	c.seq++
	seq := c.seq
	if err := syscall.Sendto(c.fd, mensajeAddIPSet(seq, set, ip, timeout), 0,
		&syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	for {
		n, _, err := syscall.Recvfrom(c.fd, buf, 0)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("ipset netlink: empty answer")
		}
		if errno, ok := errnoNetlink(buf[:n], seq); ok {
			if errno == 0 {
				return nil
			}
			// Los de ipset (4096+) no son errno del sistema.
			return &rechazoIPSet{set: set, errno: errno}
		}
	}
}
