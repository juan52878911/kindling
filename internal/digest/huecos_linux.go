package digest

import (
	"errors"
	"syscall"
)

// Los whence de lseek(2) para recorrer los huecos de un fichero disperso.
const (
	seekData = 3
	seekHole = 4
)

func esFinDeDatos(err error) bool { return errors.Is(err, syscall.ENXIO) }
