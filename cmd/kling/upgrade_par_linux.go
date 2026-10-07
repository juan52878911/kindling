//go:build linux

package main

import (
	"net"
	"syscall"
)

// pidDelPar es el PID del proceso que escucha al otro lado de c (SO_PEERCRED:
// el kernel lo apunta en listen(), no lo dice el daemon).
func pidDelPar(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if cerr != nil {
		return 0, cerr
	}
	return int(cred.Pid), nil
}
