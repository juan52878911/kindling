package contrato

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReglaAPIVersion(t *testing.T) {
	ruta := filepath.Join(t.TempDir(), "g.txt")
	cab := []string{"golden de prueba"}
	base := []string{"GET /a", "GET /b"}

	// No existe: falla sin -update; con -update se crea.
	if err := Comparar(ruta, 1, cab, base, false); err == nil {
		t.Fatal("sin golden y sin -update tiene que fallar")
	}
	if err := Comparar(ruta, 1, cab, base, true); err != nil {
		t.Fatal(err)
	}
	if err := Comparar(ruta, 1, cab, base, false); err != nil {
		t.Fatalf("igual: %v", err)
	}
	// Añadir es compatible pero pide -update.
	mas := append(base, "GET /c")
	if err := Comparar(ruta, 1, cab, mas, false); err == nil || !strings.Contains(err.Error(), "new lines") {
		t.Fatalf("añadir sin -update: %v", err)
	}
	if err := Comparar(ruta, 1, cab, mas, true); err != nil {
		t.Fatal(err)
	}
	// Quitar o cambiar sin subir el API falla incluso con -update.
	for _, ahora := range [][]string{{"GET /a", "GET /c"}, {"GET /a", "GET /b", "POST /c"}} {
		for _, upd := range []bool{false, true} {
			err := Comparar(ruta, 1, cab, ahora, upd)
			if err == nil || !strings.Contains(err.Error(), "without bumping api.APIVersion") {
				t.Fatalf("%v (update=%v): %v", ahora, upd, err)
			}
		}
	}
	// Con el API subido se puede regenerar, y hay que hacerlo.
	if err := Comparar(ruta, 2, cab, base, false); err == nil {
		t.Fatal("API subido sin regenerar tiene que fallar")
	}
	if err := Comparar(ruta, 2, cab, base, true); err != nil {
		t.Fatal(err)
	}
	ls, api, err := Leer(ruta)
	if err != nil || api != 2 || len(ls) != 2 {
		t.Fatalf("leer: %v %d %v", ls, api, err)
	}
	// Subir el API sin cambiar nada también pide regenerar la cabecera.
	if err := Comparar(ruta, 3, cab, base, false); err == nil {
		t.Fatal("cabecera vieja tiene que fallar")
	}
	b, _ := os.ReadFile(ruta)
	if !strings.HasPrefix(string(b), "# golden de prueba\n# api: 2\n") {
		t.Fatalf("cabecera: %q", b)
	}
}
