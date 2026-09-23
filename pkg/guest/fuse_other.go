//go:build !linux

package guest

import "fmt"

// Fuera de Linux no hay /dev/fuse: el protocolo se prueba contra un falso, y
// montar de verdad solo pasa dentro de la microVM.
func mountFUSE(target string, readOnly bool) (fuseDev, func(), error) {
	return nil, nil, fmt.Errorf("%w: shares only mount inside the microVM", errNoFUSE)
}
