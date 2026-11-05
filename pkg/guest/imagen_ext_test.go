package guest_test

import (
	"github.com/juan52878911/kindling/internal/imagen"
	"github.com/juan52878911/kindling/pkg/guest"
)

// TestInitEnv lee /etc/kling/env tal como lo escribe imagen.EnvFile.
func init() { guest.SetEnvFileImagen(imagen.EnvFile) }
