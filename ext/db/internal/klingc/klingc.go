// Package klingc habla con el CLI kling. Aquí solo vive la interfaz; la
// implementación que lanza el binario va en su propio fichero.
package klingc

import (
	"context"
	"io"
)

// Kling corre `kling <args...>` con stdin (puede ser nil) y devuelve su stdout.
type Kling interface {
	Run(ctx context.Context, stdin io.Reader, args ...string) (stdout []byte, err error)
}
