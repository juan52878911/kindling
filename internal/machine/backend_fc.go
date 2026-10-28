//go:build !darwin

package machine

import "fmt"

// BackendSoportado dice si este binario de kling sabe hablar con ese backend:
// fuera de macOS, solo Firecracker.
func BackendSoportado(b string) bool { return b == BackendFirecracker }

// ElegirBackend fija el backend del daemon antes de crear el Manager. Fuera
// de macOS no hay nada que elegir.
func ElegirBackend(b string) error {
	if b != BackendFirecracker {
		return fmt.Errorf("this kling binary was built for %q, not %q", BackendFirecracker, b)
	}
	return nil
}
