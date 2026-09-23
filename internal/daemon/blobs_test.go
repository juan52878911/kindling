package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

func servidorBlobs(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	mgr, err := machine.NewManager(root, "", "", events.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	return &Server{mgr: mgr, root: root}, root
}

func shaHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func putBlob(s *Server, name, part, body, sha string) *httptest.ResponseRecorder {
	u := "/images/" + name + "/blob"
	if part != "" {
		u += "?part=" + part
	}
	req := httptest.NewRequest(http.MethodPut, u, strings.NewReader(body))
	if sha != "" {
		req.Header.Set(api.HeaderSha256, sha)
	}
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	return rr
}

func sinTemporales(t *testing.T, dir string) {
	t.Helper()
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("quedó un temporal: %s", e.Name())
		}
	}
}

func TestPutYGetImageBlob(t *testing.T) {
	s, root := servidorBlobs(t)
	imgs := filepath.Join(root, "images")
	body := "rootfs de prueba"

	rr := putBlob(s, "foo", api.BlobImage, body, shaHex([]byte(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("PUT = %d %s", rr.Code, rr.Body)
	}
	if b, _ := os.ReadFile(filepath.Join(imgs, "foo.ext4")); string(b) != body {
		t.Fatalf("contenido = %q", b)
	}
	sinTemporales(t, imgs)

	// GET sin parte encuentra la monolítica y manda su hash.
	rr = httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/images/foo/blob", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != body {
		t.Fatalf("GET = %d %q", rr.Code, rr.Body)
	}
	if rr.Header().Get(api.HeaderSha256) != shaHex([]byte(body)) || rr.Header().Get(api.HeaderBlobPart) != api.BlobImage {
		t.Fatalf("cabeceras = %v", rr.Header())
	}

	// HEAD: mismas cabeceras, sin cuerpo.
	rr = httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodHead, "/images/foo/blob", nil))
	if rr.Code != http.StatusOK || rr.Body.Len() != 0 || rr.Header().Get(api.HeaderSha256) == "" {
		t.Fatalf("HEAD = %d, cuerpo %d bytes, %v", rr.Code, rr.Body.Len(), rr.Header())
	}

	// Lo mismo otra vez: idéntico, no se toca.
	rr = putBlob(s, "foo", api.BlobImage, body, "")
	var res api.BlobPutResult
	_ = json.Unmarshal(rr.Body.Bytes(), &res)
	if rr.Code != http.StatusOK || !res.Unchanged {
		t.Fatalf("PUT idéntico = %d %s", rr.Code, rr.Body)
	}
}

func TestPutImageBlobConHashMaloNoDejaNada(t *testing.T) {
	s, root := servidorBlobs(t)
	rr := putBlob(s, "foo", api.BlobImage, "datos", shaHex([]byte("otros datos")))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "corrupted") {
		t.Fatalf("PUT = %d %s", rr.Code, rr.Body)
	}
	if _, err := os.Stat(filepath.Join(root, "images", "foo.ext4")); !os.IsNotExist(err) {
		t.Fatal("una transferencia corrupta no puede dejar la imagen")
	}
	sinTemporales(t, filepath.Join(root, "images"))
}

func TestPutImageBlobNoPisaUnaImagenEnUso(t *testing.T) {
	s, root := servidorBlobs(t)
	imgs := filepath.Join(root, "images")
	if err := os.WriteFile(filepath.Join(imgs, "foo.ext4"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "snapshots", "svc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"name":"svc","image":"foo"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rr := putBlob(s, "foo", api.BlobImage, "v2", "")
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "svc") {
		t.Fatalf("PUT sobre imagen en uso = %d %s", rr.Code, rr.Body)
	}
	if b, _ := os.ReadFile(filepath.Join(imgs, "foo.ext4")); string(b) != "v1" {
		t.Fatalf("la imagen en uso cambió: %q", b)
	}
	sinTemporales(t, imgs)
	// La receta de una imagen en uso tampoco: en una por capas decide la base.
	if rr := putBlob(s, "foo", api.BlobRecipe, `{"base":"otra"}`, ""); rr.Code != http.StatusCreated {
		t.Fatalf("una receta nueva (no había) se acepta: %d %s", rr.Code, rr.Body)
	}
	if rr := putBlob(s, "foo", api.BlobRecipe, `{"base":"otra2"}`, ""); rr.Code != http.StatusConflict {
		t.Fatalf("cambiar la receta de una imagen en uso = %d", rr.Code)
	}
}

func TestImageBlobKernelYCapa(t *testing.T) {
	s, root := servidorBlobs(t)
	if rr := putBlob(s, api.KernelBlobName, "", "kernel", ""); rr.Code != http.StatusCreated {
		t.Fatalf("PUT kernel = %d %s", rr.Code, rr.Body)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "images", "vmlinux")); string(b) != "kernel" {
		t.Fatalf("vmlinux = %q", b)
	}
	if rr := putBlob(s, "svc", api.BlobLayer, "capa", ""); rr.Code != http.StatusCreated {
		t.Fatalf("PUT capa = %d %s", rr.Code, rr.Body)
	}
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/images/svc/blob", nil))
	if rr.Body.String() != "capa" || rr.Header().Get(api.HeaderBlobPart) != api.BlobLayer {
		t.Fatalf("GET de la capa = %q %v", rr.Body, rr.Header())
	}
}

func TestImageBlobValidacion(t *testing.T) {
	s, _ := servidorBlobs(t)
	casos := []struct {
		metodo, url string
		codigo      int
	}{
		{http.MethodGet, "/images/nada/blob", http.StatusNotFound},
		{http.MethodGet, "/images/Mal..Nombre/blob", http.StatusBadRequest},
		{http.MethodGet, "/images/foo/blob?part=otra", http.StatusBadRequest},
		{http.MethodPut, "/images/foo/blob", http.StatusBadRequest}, // sin parte
		{http.MethodPut, "/images/vmlinux/blob?part=image", http.StatusBadRequest},
	}
	for _, c := range casos {
		rr := httptest.NewRecorder()
		s.routes().ServeHTTP(rr, httptest.NewRequest(c.metodo, c.url, strings.NewReader("x")))
		if rr.Code != c.codigo {
			t.Errorf("%s %s = %d, quiero %d (%s)", c.metodo, c.url, rr.Code, c.codigo, rr.Body)
		}
	}
	// Un Content-Length por encima del tope se rechaza sin leer nada.
	req := httptest.NewRequest(http.MethodPut, "/images/foo/blob?part=image", strings.NewReader("x"))
	req.ContentLength = api.MaxBlobBytes + 1
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("PUT gigante = %d", rr.Code)
	}
}

func TestBuildImageSegunPlataforma(t *testing.T) {
	s, _ := servidorBlobs(t)
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/images", strings.NewReader(`{"name":"x"}`)))
	if construirImagenes {
		if rr.Code == http.StatusNotImplemented {
			t.Fatal("en Linux se construye")
		}
		return
	}
	if rr.Code != http.StatusNotImplemented || !strings.Contains(rr.Body.String(), "kling images copy") {
		t.Fatalf("POST /images en macOS = %d %s", rr.Code, rr.Body)
	}
}
