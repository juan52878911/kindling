//go:build !linux && !darwin

package machine

import (
	"fmt"
	"runtime"
)

// metodoClon: fuera de Linux y macOS no hay clon; el paquete solo tiene que
// compilar.
const metodoClon = ""

func clonarFichero(src, dst string) error {
	return fmt.Errorf("%w: not implemented on %s", errSinClon, runtime.GOOS)
}

func tipoFS(path string) string { return "" }
