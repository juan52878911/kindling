//go:build !linux && !darwin

package main

import (
	"errors"
	"net"
)

func pidDelPar(*net.UnixConn) (int, error) {
	return 0, errors.New("not supported on this system")
}
