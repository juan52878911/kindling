package machine

import (
	"runtime"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// La línea de comandos siempre lleva quiet (menos ruido de arranque en la
// consola, no menos loglevel de errores) y, solo en amd64, apaga la
// emulación de i8042: en arm64 ese controlador no existe.
func TestBootArgsQuietYArch(t *testing.T) {
	got := bootArgs([]api.VolumeAttachment{{Mount: "/data"}}, false, "")
	if !strings.Contains(got, " quiet") {
		t.Fatalf("falta quiet en la línea de arranque: %q", got)
	}

	const i8042Params = "i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd"
	switch runtime.GOARCH {
	case "amd64":
		if !strings.Contains(got, i8042Params) {
			t.Errorf("amd64 debe apagar i8042: %q", got)
		}
	default:
		if strings.Contains(got, "i8042") {
			t.Errorf("%s no debería llevar parámetros de i8042: %q", runtime.GOARCH, got)
		}
	}
}
