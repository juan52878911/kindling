package daemon

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Dos construcciones iguales a la vez (dos `kling run -image` de la misma
// referencia) hacen UNA: la segunda espera y se lleva la de la primera, en
// vez de reconstruir y renombrar el ext4 bajo las máquinas de la otra. Pedir
// lo mismo más tarde sí reconstruye.
func TestConstruccionesIgualesALaVez(t *testing.T) {
	s, h := testServer(t)
	os.MkdirAll(filepath.Join(s.root, "images"), 0o755)
	bdir := t.TempDir()
	cuenta := filepath.Join(t.TempDir(), "cuenta")
	t.Setenv("KLING_BUILDERS_DIR", bdir)
	t.Setenv("KLING_BUILDERS_INSECURE", "1")
	t.Setenv("KLING_TEST_CUENTA", cuenta)
	instalarConstructor(t, bdir, "oci", `
set -e
echo "$KLING_IMAGE_NAME" >> "$KLING_TEST_CUENTA"
sleep 0.3
mkdir -p "$KLING_OUT_DIR"
echo raiz > "$KLING_OUT_DIR/$KLING_IMAGE_NAME.ext4"
`)
	veces := func() int {
		b, _ := os.ReadFile(cuenta)
		return strings.Count(string(b), "\n")
	}
	pedir := func(bodies ...string) []int {
		codes := make([]int, len(bodies))
		var wg sync.WaitGroup
		for i, b := range bodies {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, httptest.NewRequest("POST", "/images", strings.NewReader(b)))
				codes[i] = rr.Code
			}()
		}
		wg.Wait()
		return codes
	}
	igual := `{"name":"pg","builder":"oci","spec":{"ref":"postgres:16"}}`
	// El mismo spec con otros bytes cuenta como igual.
	igual2 := `{"name":"pg","builder":"oci","spec":{ "ref" : "postgres:16" }}`
	for _, c := range pedir(igual, igual2, igual, igual2) {
		if c != 200 {
			t.Fatalf("códigos: %d", c)
		}
	}
	if n := veces(); n != 1 {
		t.Fatalf("cuatro peticiones iguales a la vez construyeron %d veces, no 1", n)
	}

	// Más tarde, lo mismo otra vez: se reconstruye (-rebuild, una etiqueta
	// que se movió).
	pedir(igual)
	if n := veces(); n != 2 {
		t.Fatalf("una petición posterior tiene que reconstruir: %d", n)
	}
	// A la vez, pero con otro spec: las dos.
	pedir(igual, `{"name":"pg","builder":"oci","spec":{"ref":"postgres:17"}}`)
	if n := veces(); n != 4 {
		t.Fatalf("con otro spec se construye: %d", n)
	}
}
