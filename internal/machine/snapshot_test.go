package machine

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// El error de TSC es el peor tipo de fallo: determinista, masivo (un reinicio
// del host invalida TODOS los snapshots a la vez) y con un mensaje de
// Firecracker que no apunta a ninguna salida. Traducirlo es la diferencia entre
// un 502 críptico y saber que hay que rehacer el snapshot.
func TestElErrorDeTSCSeTraduceAAlgoAccionable(t *testing.T) {
	// Tal y como llega de verdad, envuelto por el cliente de fc.
	crudo := fmt.Errorf("firecracker PUT /snapshot/load: 400 Bad Request: " +
		"Load snapshot error: Failed to restore from snapshot: " +
		"Could not set TSC scaling within the snapshot: Invalid argument (os error 22)")

	err := explainRestoreErr(crudo, `snapshot "github"`, "  kling mcp import github -force")
	if err == nil {
		t.Fatal("se tragó el error")
	}
	// Tiene que decir qué pasó (el reinicio del host) y qué hacer (rehacerlo).
	for _, q := range []string{"reboot", "kling mcp import github -force", `snapshot "github"`} {
		if !strings.Contains(err.Error(), q) {
			t.Errorf("la traducción no menciona %q:\n%v", q, err)
		}
	}
	// Y conserva el error original: quien depure necesita el texto de verdad.
	if !strings.Contains(err.Error(), "Could not set TSC scaling") {
		t.Error("la traducción perdió el error original de Firecracker")
	}

	// Lo que no se entiende pasa INTACTO: adornar un error sin conocer su causa
	// es peor que dejarlo crudo.
	otro := errors.New("firecracker PUT /snapshot/load: 400 Bad Request: missing mem file")
	if got := explainRestoreErr(otro, "snapshot \"x\"", "whatever"); got != otro {
		t.Errorf("alteró un error que no reconoce: %v", got)
	}
	if got := explainRestoreErr(nil, "snapshot \"x\"", "whatever"); got != nil {
		t.Errorf("inventó un error donde no lo había: %v", got)
	}
}
