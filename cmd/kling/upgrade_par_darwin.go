//go:build darwin

package main

import (
	"net"
	"syscall"
)

// pidDelPar es el PID del proceso que escucha al otro lado de c
// (LOCAL_PEERPID de <sys/un.h>: SOL_LOCAL 0, LOCAL_PEERPID 2).
func pidDelPar(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var serr error
	if err := raw.Control(func(fd uintptr) {
		pid, serr = syscall.GetsockoptInt(int(fd), 0, 2)
	}); err != nil {
		return 0, err
	}
	return pid, serr
}
