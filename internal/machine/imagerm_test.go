package machine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/api"
)

func conImagen(t *testing.T, m *Manager, nombre string, capa bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(m.root, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	ruta := m.imagePath(nombre)
	if capa {
		ruta = m.layerPath(nombre)
	}
	if err := os.WriteFile(ruta, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Borrar la BASE de una capa no da un error al borrar: da un invitado que no
// arranca, mucho después, porque su rootfs ya no está. Por eso se comprueba.
func TestNoSePuedeBorrarLaBaseDeUnaCapa(t *testing.T) {
	m := newTestManager(t)
	conImagen(t, m, "base", false)
	conImagen(t, m, "encima", true)
	// La receta es lo que dice sobre qué base se construyó.
	if err := os.WriteFile(m.recipePath("encima"),
		[]byte(`{"name":"encima","base":"base","cmd":["x"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	err := m.RemoveImage("base")
	if err == nil {
		t.Fatal("se borró la base de una capa viva")
	}
	if !strings.Contains(err.Error(), "encima") {
		t.Errorf("el error no dice quién depende de ella: %v", err)
	}
	if _, e := os.Stat(m.imagePath("base")); e != nil {
		t.Error("la base se borró de todas formas")
	}
}

// Ni la imagen de la que cuelga un dorado.
func TestNoSePuedeBorrarLaImagenDeUnDorado(t *testing.T) {
	m := newTestManager(t)
	conImagen(t, m, "usada", false)
	dir := filepath.Join(m.root, "snapshots", "un-servicio")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"),
		[]byte(`{"name":"un-servicio","image":"usada"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	err := m.RemoveImage("usada")
	if err == nil {
		t.Fatal("se borró la imagen de un dorado")
	}
	if !strings.Contains(err.Error(), "un-servicio") {
		t.Errorf("el error no dice qué dorado la usa: %v", err)
	}
}

// Ni una que esté arrancada ahora mismo.
func TestNoSePuedeBorrarUnaImagenEnUso(t *testing.T) {
	m := newTestManager(t)
	conImagen(t, m, "corriendo", false)
	m.byID["x"] = &api.Machine{ID: "x", Name: "viva", Image: "corriendo", State: api.StateRunning}

	if err := m.RemoveImage("corriendo"); err == nil {
		t.Error("se borró una imagen con una máquina viva encima")
	}
}

// Y la que no usa nadie SÍ se borra, con su receta.
func TestUnaImagenSinDependientesSeBorraConSuReceta(t *testing.T) {
	m := newTestManager(t)
	conImagen(t, m, "sobrante", false)
	if err := os.WriteFile(m.recipePath("sobrante"), []byte(`{"name":"sobrante"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.RemoveImage("sobrante"); err != nil {
		t.Fatalf("RemoveImage: %v", err)
	}
	for _, p := range []string{m.imagePath("sobrante"), m.recipePath("sobrante")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("quedó %s", p)
		}
	}
	// Y una que no existe lo dice, en vez de callar.
	if err := m.RemoveImage("no-existe"); err == nil {
		t.Error("borrar algo inexistente no dio error")
	}
}
