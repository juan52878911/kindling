package machine

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// S10: con una plantilla ya ocupando el nombre de una del snapshot, el 409
// decía que existía y cómo borrarla (`kling template rm`), aunque fuera de
// otro inquilino. Ahora solo dice que el nombre está ocupado.
func TestGrafoSnapshotNombreOcupadoNeutro(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	e.montarGrafo(grafoTienda(true))
	if err := os.MkdirAll(e.m.snapDir("tienda-web-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := e.m.GraphSnapshot(context.Background(), "tienda", "")
	var se *api.StatusError
	if !errors.As(err, &se) || se.Code != 409 {
		t.Fatalf("esperaba un 409, llegó %v", err)
	}
	if !strings.Contains(se.Message, "is taken") || strings.Contains(se.Message, "already exists") || strings.Contains(se.Message, "template rm") {
		t.Fatalf("el 409 dice más que «ocupado»: %q", se.Message)
	}
}
