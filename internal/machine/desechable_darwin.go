//go:build darwin

package machine

import (
	"syscall"
	"time"
)

// arranqueHost es la hora a la que arrancó el Mac (kern.boottime, un struct
// timeval: los ocho primeros bytes son los segundos).
func arranqueHost() (time.Time, bool) {
	s, err := syscall.Sysctl("kern.boottime")
	if err != nil || len(s) == 0 {
		return time.Time{}, false
	}
	seg := int64(leerUint64LE(s))
	if seg <= 0 {
		return time.Time{}, false
	}
	return time.Unix(seg, 0), true
}
