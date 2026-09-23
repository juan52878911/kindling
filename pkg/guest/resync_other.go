//go:build !linux

package guest

import (
	"errors"
	"time"
)

// Fuera de Linux no hay invitado que resincronizar: estas versiones existen
// para que el paquete compile en el Mac donde se desarrolla.

var errResyncOS = errors.New("resync only works inside a Linux guest")

func setClockOS(time.Time) error  { return errResyncOS }
func mixEntropyOS(b []byte) error { return errResyncOS }
