package guest

// envFileImagen es internal/imagen EnvFile. Lo pone imagen_ext_test.go (fuera
// del paquete): internal/imagen importa pkg/guest, y una prueba de dentro que
// lo importara haría un ciclo.
var envFileImagen func(env []string) string

// SetEnvFileImagen lo usa imagen_ext_test.go.
func SetEnvFileImagen(f func(env []string) string) { envFileImagen = f }
