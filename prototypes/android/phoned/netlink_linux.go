package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"syscall"
	"time"
)

// nlConn es un socket NETLINK_ROUTE del espacio de red del hilo que lo abrió.
type nlConn struct {
	fd  int
	seq uint32
}

func openNL() (*nlConn, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("netlink socket: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("netlink bind: %w", err)
	}
	tv := syscall.NsecToTimeval(int64(5 * time.Second))
	_ = syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
	return &nlConn{fd: fd, seq: uint32(time.Now().UnixNano())}, nil
}

func (c *nlConn) Close() { syscall.Close(c.fd) }

// do manda un mensaje y espera su ACK; un error del kernel vuelve como Errno.
func (c *nlConn) do(what string, build func(seq uint32) []byte) error {
	c.seq++
	seq := c.seq
	if err := syscall.Sendto(c.fd, build(seq), 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return fmt.Errorf("%s: send: %w", what, err)
	}
	buf := make([]byte, 8192)
	for {
		n, _, err := syscall.Recvfrom(c.fd, buf, 0)
		if err != nil {
			return fmt.Errorf("%s: recv: %w", what, err)
		}
		if e, ok := nlAckErr(buf[:n], seq); ok {
			if e != 0 {
				return fmt.Errorf("%s: %w", what, syscall.Errno(-e))
			}
			return nil
		}
	}
}

func setns(fd int, nstype int) error {
	_, _, e := syscall.RawSyscall(sysSetns, uintptr(fd), uintptr(nstype), 0)
	if e != 0 {
		return e
	}
	return nil
}

// inNetns ejecuta fn en un hilo del sistema metido en el espacio de red nsFD.
// Lo que fn abra (sockets) se queda en ese espacio aunque el hilo vuelva al
// suyo. Si no se puede volver, el hilo se tira (no se desbloquea): Go lo
// termina al acabar la gorrutina en vez de reutilizarlo en el espacio ajeno.
func inNetns(nsFD int, fn func() error) error {
	runtime.LockOSThread()
	orig, err := os.Open("/proc/thread-self/ns/net")
	if err != nil {
		runtime.UnlockOSThread()
		return err
	}
	defer orig.Close()
	if err := setns(nsFD, syscall.CLONE_NEWNET); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("setns: %w", err)
	}
	ferr := fn()
	if err := setns(int(orig.Fd()), syscall.CLONE_NEWNET); err != nil {
		return errors.Join(ferr, fmt.Errorf("setns back: %w", err))
	}
	runtime.UnlockOSThread()
	return ferr
}

// linkIndex es el índice de un enlace del espacio de red del hilo actual.
func linkIndex(name string) (int, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return 0, err
	}
	return ifi.Index, nil
}

// vethSetup crea el enlace veth de Android: hostIf en la VM con hostIP/30 y
// eth0 en el espacio de red del proceso androidPID (su netns es nsFD) con
// androidIP/30, lo de dentro con lo arriba y ruta por defecto por hostIP.
// Todo por netlink; lo único que no es netlink es ip_forward (/proc).
func vethSetup(hostIf string, hostIP, androidIP net.IP, androidPID, nsFD int) error {
	nl, err := openNL()
	if err != nil {
		return err
	}
	defer nl.Close()
	// Uno que quedara de un Android anterior muere con su espacio de red, pero
	// si el espacio lo retuviera algo, el nombre estaría ocupado.
	_ = nl.do("del "+hostIf, func(s uint32) []byte { return msgDelLink(s, hostIf) })
	if err := nl.do("veth "+hostIf, func(s uint32) []byte { return msgNewVeth(s, hostIf, "eth0", androidPID) }); err != nil {
		return err
	}
	idx, err := linkIndex(hostIf)
	if err != nil {
		return err
	}
	if err := nl.do("addr "+hostIf, func(s uint32) []byte { return msgAddAddr(s, idx, hostIP, 30) }); err != nil {
		return err
	}
	if err := nl.do("up "+hostIf, func(s uint32) []byte { return msgLinkUp(s, idx) }); err != nil {
		return err
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("ip_forward: %w", err)
	}
	return inNetns(nsFD, func() error {
		in, err := openNL()
		if err != nil {
			return err
		}
		defer in.Close()
		if err := linkUpByName(in, "lo"); err != nil {
			return err
		}
		eth, err := linkIndex("eth0")
		if err != nil {
			return fmt.Errorf("eth0 in the Android netns: %w", err)
		}
		if err := in.do("addr eth0", func(s uint32) []byte { return msgAddAddr(s, eth, androidIP, 30) }); err != nil {
			return err
		}
		if err := in.do("up eth0", func(s uint32) []byte { return msgLinkUp(s, eth) }); err != nil {
			return err
		}
		return in.do("default route", func(s uint32) []byte { return msgDefaultRoute(s, eth, hostIP) })
	})
}

func linkUpByName(nl *nlConn, name string) error {
	idx, err := linkIndex(name)
	if err != nil {
		return err
	}
	return nl.do("up "+name, func(s uint32) []byte { return msgLinkUp(s, idx) })
}

// loUp sube lo en el espacio de red nsFD (modo isolated).
func loUp(nsFD int) error {
	return inNetns(nsFD, func() error {
		nl, err := openNL()
		if err != nil {
			return err
		}
		defer nl.Close()
		return linkUpByName(nl, "lo")
	})
}
