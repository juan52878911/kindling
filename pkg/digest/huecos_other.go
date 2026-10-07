//go:build !linux && !darwin

package digest

// Sin SEEK_DATA: se leen todos los trozos.
const (
	seekData = -1
	seekHole = -1
)

func esFinDeDatos(error) bool { return false }
