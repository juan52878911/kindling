package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// conCerrojo retiene ConImagenesQuietas hasta que se llame a la función que
// devuelve (que espera a que lo suelte).
func conCerrojo(t *testing.T, s *Server) func() {
	t.Helper()
	dentro, salir, fuera := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		_ = s.mgr.ConImagenesQuietas(func() error {
			close(dentro)
			<-salir
			return nil
		})
		close(fuera)
	}()
	<-dentro
	return func() { close(salir); <-fuera }
}

// putEnVuelo lanza un PUT que tiene que quedarse esperando al cerrojo de las
// imágenes, y comprueba que espera.
func putEnVuelo(t *testing.T, s *Server, name, body string) <-chan *httptest.ResponseRecorder {
	t.Helper()
	hecho := make(chan *httptest.ResponseRecorder, 1)
	go func() { hecho <- putBlob(s, name, api.BlobImage, body, "") }()
	select {
	case rr := <-hecho:
		t.Fatalf("el PUT contestó sin esperar a las imágenes quietas: %d %s", rr.Code, rr.Body)
	case <-time.After(300 * time.Millisecond):
	}
	return hecho
}

// La comprobación de uso y el rename van DENTRO de ConImagenesQuietas: un uso
// que aparece mientras el PUT espera (aquí un dorado; en la vida real también
// un arranque en vuelo, que ImageReplaceable ve: TestSustituirConArranqueEnVuelo
// en machine) se ve y da 409, y la imagen no cambia mientras se espera.
func TestPutImageBlobSustituyeConImagenesQuietas(t *testing.T) {
	s, root := servidorBlobs(t)
	imgs := filepath.Join(root, "images")
	img := filepath.Join(imgs, "foo.ext4")
	if err := os.WriteFile(img, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	soltar := conCerrojo(t, s)
	hecho := putEnVuelo(t, s, "foo", "v2")
	if b, _ := os.ReadFile(img); string(b) != "v1" {
		t.Fatalf("la imagen cambió fuera del cerrojo: %q", b)
	}
	dir := filepath.Join(root, "snapshots", "svc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"name":"svc","image":"foo"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	soltar()
	if rr := <-hecho; rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "svc") {
		t.Fatalf("PUT sobre una imagen que pasó a estar en uso mientras esperaba = %d %s, quería 409", rr.Code, rr.Body)
	}
	if b, _ := os.ReadFile(img); string(b) != "v1" {
		t.Fatalf("la imagen en uso cambió: %q", b)
	}
	sinTemporales(t, imgs)
}

// "Idéntico: 200 sin tocar nada" también se decide dentro del cerrojo: si lo
// que había cambió mientras el PUT esperaba, ya no es idéntico y se sustituye.
func TestPutImageBlobIdenticoSeMiraDentroDelCerrojo(t *testing.T) {
	s, root := servidorBlobs(t)
	imgs := filepath.Join(root, "images")
	img := filepath.Join(imgs, "foo.ext4")
	if rr := putBlob(s, "foo", api.BlobImage, "v1", ""); rr.Code != http.StatusCreated {
		t.Fatalf("PUT v1 = %d %s", rr.Code, rr.Body)
	}
	soltar := conCerrojo(t, s)
	hecho := putEnVuelo(t, s, "foo", "v1")
	// Otra subida cambia la imagen mientras este PUT espera.
	if err := os.WriteFile(img+".nuevo", []byte("v3"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(img+".nuevo", img); err != nil {
		t.Fatal(err)
	}
	soltar()
	rr := <-hecho
	var res api.BlobPutResult
	_ = json.Unmarshal(rr.Body.Bytes(), &res)
	if rr.Code != http.StatusCreated || res.Unchanged {
		t.Fatalf("PUT de v1 sobre v3 = %d %s, quería 201 sustituyendo", rr.Code, rr.Body)
	}
	if b, _ := os.ReadFile(img); string(b) != "v1" {
		t.Fatalf("imagen = %q, quería v1", b)
	}
	// Sin carreras, idéntico sigue siendo 200 sin tocar nada.
	if rr := putBlob(s, "foo", api.BlobImage, "v1", ""); rr.Code != http.StatusOK {
		t.Fatalf("PUT idéntico = %d %s, quería 200", rr.Code, rr.Body)
	}
	sinTemporales(t, imgs)
}
