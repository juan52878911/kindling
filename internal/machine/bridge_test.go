package machine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/api"
)

// Nunca se toca la imagen base de una microVM viva.
//
// Es la comprobación que evita el desastre: modificar en escritura un ext4 que
// otro sistema tiene montado lo corrompe, aunque él lo tenga en solo lectura. Y
// aquí "vivo" incluye a las warm: al descongelarse vuelven a leer de la imagen,
// que además está mapeada en su snapshot de memoria.
func TestNoSeRefrescaLaImagenDeUnaMaquinaViva(t *testing.T) {
	m := newTestManager(t)
	imgs := filepath.Join(m.root, "images")
	if err := os.MkdirAll(imgs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"ocupada", "libre"} {
		if err := os.WriteFile(filepath.Join(imgs, n+".ext4"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	puente := filepath.Join(t.TempDir(), "kling-bridge")
	if err := os.WriteFile(puente, []byte("binario"), 0o755); err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	m.byID["a"] = &api.Machine{ID: "a", Name: "svc-a", State: api.StateWarm, Image: "ocupada"}
	m.mu.Unlock()

	res, err := m.RefreshBridges(t.Context(), puente, nil)
	if err != nil {
		t.Fatal(err)
	}
	porImagen := map[string]api.BridgeRefresh{}
	for _, r := range res {
		porImagen[r.Image] = r
	}

	ocupada, ok := porImagen["ocupada"]
	if !ok {
		t.Fatal("no informó de la imagen ocupada")
	}
	if !ocupada.Skipped {
		t.Error("iba a tocar la imagen de una máquina viva")
	}
	if ocupada.Updated {
		t.Error("dice que la actualizó pese a saltarla")
	}
	// Y tiene que decir QUIÉN la retiene, o el usuario no sabe qué parar.
	if !strings.Contains(ocupada.Error, "svc-a") {
		t.Errorf("no dice quién la usa: %q", ocupada.Error)
	}

	// La libre sí se intenta (fallará al montar, que aquí no hay loop, pero no
	// se salta).
	if porImagen["libre"].Skipped {
		t.Error("saltó una imagen que no usa nadie")
	}
}

// Sin puente que inyectar, se dice DÓNDE debería estar en vez de fallar seco.
func TestSinPuenteExplicaDondeBuscarlo(t *testing.T) {
	m := newTestManager(t)
	_, err := m.RefreshBridges(t.Context(), "", nil)
	if err == nil {
		t.Fatal("debería fallar")
	}
	if !strings.Contains(err.Error(), "/usr/local/lib/kindling") {
		t.Errorf("el error no dice dónde debería estar: %v", err)
	}
}

// overlay-template no es una imagen de rootfs: es el molde vacío del disco de
// escritura de cada máquina y no lleva puente dentro. Intentar refrescarlo daría
// un fallo confuso en cada ejecución.
func TestElMoldeDeOverlayNoEsUnaImagen(t *testing.T) {
	m := newTestManager(t)
	imgs := filepath.Join(m.root, "images")
	if err := os.MkdirAll(imgs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"servicio", "overlay-template"} {
		if err := os.WriteFile(filepath.Join(imgs, n+".ext4"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := m.Images()
	if len(got) != 1 || got[0] != "servicio" {
		t.Errorf("Images() = %v, quería solo [servicio]", got)
	}
}

// Comparar por CONTENIDO y no por fecha o tamaño: dos puentes distintos pueden
// pesar lo mismo, y una imagen reescrita sin necesidad pierde su dispersión en
// disco.
func TestSeComparaPorContenido(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.WriteFile(a, []byte("mismo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("mismo"), 0o755); err != nil {
		t.Fatal(err)
	}
	da, err := fileDigest(a)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := fileDigest(b)
	if da != db {
		t.Error("dos ficheros idénticos dieron huellas distintas")
	}
	if err := os.WriteFile(b, []byte("otro!"), 0o755); err != nil {
		t.Fatal(err)
	}
	if dc, _ := fileDigest(b); dc == da {
		t.Error("dos ficheros distintos del mismo tamaño dieron la misma huella")
	}
}
