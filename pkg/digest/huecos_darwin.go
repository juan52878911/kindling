package digest

import (
	"errors"
	"syscall"
)

// En macOS van al revés que en Linux (sys/unistd.h).
const (
	seekHole = 3
	seekData = 4
)

func esFinDeDatos(err error) bool { return errors.Is(err, syscall.ENXIO) }
