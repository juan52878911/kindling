//go:build !linux

package guest

import (
	"errors"
	"net"
)

// ListenVsock solo existe dentro de una microVM (Linux).
func ListenVsock(uint32) (net.Listener, error) {
	return nil, errors.New("vsock is only available on Linux")
}
