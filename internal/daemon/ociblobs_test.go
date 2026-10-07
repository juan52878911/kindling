package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func putOCIBlob(s *Server, digest string, body io.Reader, size int64) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/oci/blobs/"+digest, body)
	req.ContentLength = size
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	return rr
}

func getOCIBlob(s *Server, digest string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/oci/blobs/"+digest, nil))
	return rr
}

// Lo que sube el CLI de un archivo queda en la caché de blobs OCI solo si es
// el contenido de su digest, y nunca a medias.
func TestPutOCIBlob(t *testing.T) {
	s, root := servidorBlobs(t)
	dir := filepath.Join(root, "cache", "oci", "sha256")
	body := "una capa de docker save"
	d := "sha256:" + shaHex([]byte(body))
	p := filepath.Join(dir, shaHex([]byte(body)))

	if rr := getOCIBlob(s, d); rr.Code != http.StatusNotFound {
		t.Fatalf("GET before = %d", rr.Code)
	}
	// Otro contenido con ese digest: no.
	if rr := putOCIBlob(s, d, strings.NewReader(strings.ToUpper(body)), int64(len(body))); rr.Code != http.StatusBadRequest ||
		!strings.Contains(rr.Body.String(), "sha256 mismatch") {
		t.Fatalf("wrong content = %d %s", rr.Code, rr.Body)
	}
	// Menos de lo que dice Content-Length: no.
	if rr := putOCIBlob(s, d, strings.NewReader(body[:5]), int64(len(body))); rr.Code != http.StatusBadRequest {
		t.Fatalf("truncated = %d %s", rr.Code, rr.Body)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("a rejected blob is in the cache: %v", err)
	}
	if es, _ := os.ReadDir(dir); len(es) != 0 {
		t.Fatalf("leftovers: %v", es)
	}

	rr := putOCIBlob(s, d, strings.NewReader(body), int64(len(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("PUT = %d %s", rr.Code, rr.Body)
	}
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("blob %v %v", fi, err)
	}
	if b, _ := os.ReadFile(p); string(b) != body {
		t.Fatalf("content %q", b)
	}
	rr = getOCIBlob(s, d)
	var got api.OCIBlob
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &got) != nil || got.Size != int64(len(body)) {
		t.Fatalf("GET = %d %s", rr.Code, rr.Body)
	}
	// Ya está: no se lee nada.
	rr = putOCIBlob(s, d, strings.NewReader("no se lee"), int64(len(body)))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"unchanged":true`) {
		t.Fatalf("again = %d %s", rr.Code, rr.Body)
	}
}

func TestPutOCIBlobRejects(t *testing.T) {
	s, root := servidorBlobs(t)
	ok := "sha256:" + strings.Repeat("a", 64)
	for _, c := range []struct {
		name, digest string
		size         int64
		code         int
	}{
		{"no digest", "x", 1, http.StatusBadRequest},
		{"md5", "md5:" + strings.Repeat("a", 32), 1, http.StatusBadRequest},
		{"uppercase", "sha256:" + strings.Repeat("A", 64), 1, http.StatusBadRequest},
		{"dotdot", "..%2F..%2Fimages%2Fmin", 1, http.StatusBadRequest},
		{"no length", ok, -1, http.StatusLengthRequired},
		{"too big", ok, api.MaxBlobBytes + 1, http.StatusRequestEntityTooLarge},
	} {
		t.Run(c.name, func(t *testing.T) {
			if rr := putOCIBlob(s, c.digest, strings.NewReader("x"), c.size); rr.Code != c.code {
				t.Fatalf("= %d %s, want %d", rr.Code, rr.Body, c.code)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(root, "cache", "oci", "sha256")); !os.IsNotExist(err) {
		t.Fatalf("a rejected upload created the cache: %v", err)
	}
}

// Un enlace con el nombre de un blob no cuenta como "ya está".
func TestOCIBlobNotALink(t *testing.T) {
	s, root := servidorBlobs(t)
	dir := filepath.Join(root, "cache", "oci", "sha256")
	os.MkdirAll(dir, 0o755)
	body := "contenido"
	h := shaHex([]byte(body))
	os.WriteFile(filepath.Join(root, "otro"), []byte(body), 0o644)
	os.Symlink(filepath.Join(root, "otro"), filepath.Join(dir, h))
	if rr := getOCIBlob(s, "sha256:"+h); rr.Code != http.StatusNotFound {
		t.Fatalf("GET of a link = %d", rr.Code)
	}
	if rr := putOCIBlob(s, "sha256:"+h, strings.NewReader(body), int64(len(body))); rr.Code != http.StatusCreated {
		t.Fatalf("PUT over a link = %d %s", rr.Code, rr.Body)
	}
	if fi, err := os.Lstat(filepath.Join(dir, h)); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("the link stayed: %v", err)
	}
}
