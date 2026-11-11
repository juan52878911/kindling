//go:build e2etest

package upgrade

// claveE2E es una clave pública más, solo en las compilaciones del e2e
// (go build -tags e2etest -ldflags "-X …/internal/upgrade.claveE2E=RW…").
// Desde v1.0.0 kling upgrade solo instala releases firmadas, y el e2e de
// actualización (scripts/94-e2e-upgrade.sh, modo de descarga) sirve una
// release local que firma con una clave de usar y tirar que genera el CI. Un
// binario sin la etiqueta e2etest no compila este fichero: sus claves son
// solo las de clavesRelease.
var claveE2E string

func init() {
	if claveE2E != "" {
		clavesRelease = append(clavesRelease, claveE2E)
	}
}
