//go:build darwin

package transport

import (
	"os"
	"path/filepath"
)

// DefaultRoot es donde guarda sus datos el daemon local. En macOS el daemon
// corre como el usuario, sin root, así que no puede usar /var/lib: va al
// directorio de datos de aplicación del usuario, que además Time Machine y
// Spotlight ya saben tratar.
func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "kindling")
	}
	return filepath.Join(home, "Library", "Application Support", "kindling")
}

// DefaultSocketPath es el socket del daemon local. Va dentro de la raíz por
// defecto (no de KLING_ROOT) para que el CLI lo encuentre sin saber con qué
// raíz se arrancó el daemon. /run no existe en macOS y no es del usuario.
func DefaultSocketPath() string { return filepath.Join(DefaultRoot(), "kling.sock") }
