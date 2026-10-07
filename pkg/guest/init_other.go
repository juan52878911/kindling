//go:build !linux

package guest

import "errors"

// Fuera de Linux no hay invitado que arrancar: existe para que el paquete
// compile en el Mac donde se desarrolla.
func osInitSys() (initSys, error) {
	return nil, errors.New("the init only runs inside a Linux guest")
}
