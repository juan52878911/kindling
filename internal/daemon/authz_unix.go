//go:build linux || darwin

package daemon

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// abrirSinEnlace abre ruta para leer sin seguir un enlace simbólico en el
// último componente (O_NOFOLLOW) y sin bloquearse si es una FIFO
// (O_NONBLOCK): lo que se comprueba después con Stat es el descriptor que se
// va a leer, no un nombre que podría cambiar entre medias.
func abrirSinEnlace(ruta string) (*os.File, error) {
	f, err := os.OpenFile(ruta, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ELOOP) {
		return nil, fmt.Errorf("%s: is a symbolic link", ruta)
	}
	return f, err
}
