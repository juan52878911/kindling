package daemon

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// El mux desescapa %2F antes de casar el patrón: sin el filtro,
// DELETE /volumes/..%2Fimages%2Fmin borraba la imagen base, y lo mismo con
// cualquier otro {name} o {ref}. Tras el arreglo, 400 y el fichero sigue ahí.
func TestRutasConBarraEscapadaSeRechazan(t *testing.T) {
	s, root := servidorBlobs(t)
	base := filepath.Join(root, "images", "min.ext4")
	if err := os.WriteFile(base, []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ metodo, url string }{
		{http.MethodDelete, "/volumes/..%2Fimages%2Fmin"},
		{http.MethodDelete, "/volumes/..%2fimages%2fmin"},
		{http.MethodDelete, "/images/..%2Fimages%2Fmin"},
		{http.MethodGet, "/images/..%2Fvolumes%2Fdatos/files?path=/etc/passwd"},
		{http.MethodDelete, "/snapshots/..%2F..%2Fetc"},
		{http.MethodDelete, "/machines/..%5Cx"},
	} {
		rr := httptest.NewRecorder()
		s.routes().ServeHTTP(rr, httptest.NewRequest(c.metodo, c.url, nil))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s %s = %d, quería 400", c.metodo, c.url, rr.Code)
		}
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("la imagen base desapareció: %v", err)
	}
}

// Las rutas normales siguen llegando a su handler.
func TestRutasNormalesPasanElFiltro(t *testing.T) {
	s, _ := servidorBlobs(t)
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/volumes/no-existe", nil))
	if rr.Code == http.StatusBadRequest {
		t.Fatalf("DELETE /volumes/no-existe = 400: el filtro corta rutas legítimas (%s)", rr.Body)
	}
}
