//go:build !linux && !darwin

package daemon

import (
	"errors"
	"net"
	"os"
)

// credencialesPar: fuera de Linux y macOS no se sabe leer quién llama, así
// que con política nadie tiene rol (se deniega todo) y sin ella todo sigue
// como siempre.
func credencialesPar(net.Conn) (Llamante, error) {
	return Llamante{}, errors.New("peer credentials are not supported on this system")
}

func dueñoFichero(os.FileInfo) (int, bool) { return 0, false }

// abrirSinEnlace: sin O_NOFOLLOW portable, la política no se carga (tampoco
// se sabría su dueño).
func abrirSinEnlace(string) (*os.File, error) {
	return nil, errors.New("authz policies are not supported on this system")
}
