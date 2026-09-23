//go:build !linux && !darwin

package share

import (
	"errors"
	"os"
	"syscall"
	"time"

	proto "github.com/juan52878911/kindling/pkg/share"
)

// El daemon solo corre en Linux y macOS. Esto existe para que el paquete
// compile en cualquier otro sitio, no para usarse.

var errUnsupported = errors.New("shares are not supported on this system")

func statTimes(st *syscall.Stat_t) (time.Time, time.Time) { return time.Time{}, time.Time{} }

func renameat(*os.File, string, *os.File, string) error { return errUnsupported }
func readlinkat(*os.File, string) (string, error)       { return "", errUnsupported }
func fstatfs(*os.File) (proto.Statfs, error)            { return proto.Statfs{}, errUnsupported }
