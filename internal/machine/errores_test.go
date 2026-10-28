package machine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// El daemon decide el código HTTP con errors.Is: cada error de borrado de
// imagen o snapshot tiene que llevar su centinela, y el texto de siempre.
func TestCentinelasDeImagenYSnapshot(t *testing.T) {
	m := newTestManager(t)
	if err := os.MkdirAll(filepath.Join(m.root, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.imagePath("usada"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	escribirSnapshot(t, m, "dorado", api.Snapshot{Image: "usada"})

	casos := []struct {
		nombre string
		err    error
		marca  error
		texto  string
	}{
		{"imagen inválida", m.RemoveImage("../x"), ErrInvalidName, `invalid image name "../x"`},
		{"imagen que no existe", m.RemoveImage("nada"), ErrNoImage, `image "nada" does not exist`},
		{"imagen de un dorado", m.RemoveImage("usada"), ErrImageInUse,
			`image "usada" is used by the golden snapshot "dorado": re-import or remove that service first`},
		{"snapshot inválido", m.RemoveSnapshot("../x"), ErrInvalidName, `invalid snapshot name: "../x"`},
		{"snapshot que no existe", m.RemoveSnapshot("nada"), ErrNoSnapshot, `snapshot "nada" does not exist`},
	}
	_, err := m.Snapshot("nada")
	casos = append(casos, struct {
		nombre string
		err    error
		marca  error
		texto  string
	}{"Snapshot que no existe", err, ErrNoSnapshot, `snapshot "nada" does not exist`})

	liberar := m.reserveDir(reservaSnapshot("dorado"))
	err = m.RemoveSnapshot("dorado")
	liberar()
	if !errors.Is(err, ErrSnapshotInUse) {
		t.Errorf("snapshot reservado: %v, quería ErrSnapshotInUse", err)
	}

	for _, c := range casos {
		if !errors.Is(c.err, c.marca) {
			t.Errorf("%s: %v no es %v", c.nombre, c.err, c.marca)
			continue
		}
		if c.err.Error() != c.texto {
			t.Errorf("%s: el texto cambió: %q, era %q", c.nombre, c.err, c.texto)
		}
	}
}
