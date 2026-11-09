//go:build !linux

package fc

import (
	"context"
	"net"
)

// dialVMM fuera de Linux es dialUnix: no hay jailer, y el VMM de macOS
// (kling-vz, kling-hv) va confinado por su propio perfil. Ver par.go.
func dialVMM(ctx context.Context, path string) (net.Conn, error) {
	return dialUnix(ctx, path)
}
