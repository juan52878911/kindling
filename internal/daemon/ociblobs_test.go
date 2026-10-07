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
	"time"

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
	// Sin usuario de construcción, solo para root (para el daemon).
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("blob %v %v", fi, err)
	}
	for _, d := range []string{dir, filepath.Dir(dir)} {
		if fi, err := os.Lstat(d); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", d, fi, err)
		}
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

// Lo subido no es legible para las demás cuentas del host (una imagen
// privada) ni para el constructor: solo de root, directorios 0700 y blobs
// 0600; lo de una versión anterior (0755 y 0644, o 0750 y 0640 del grupo del
// constructor) se cierra al subir.
func TestPutOCIBlobSinLecturaParaOtros(t *testing.T) {
	s, root := servidorBlobs(t)
	s.constructor = yo()
	dir := filepath.Join(root, "cache", "oci", "sha256")
	os.MkdirAll(dir, 0o755)
	os.Chmod(filepath.Dir(dir), 0o755)
	viejo := filepath.Join(dir, strings.Repeat("b", 64))
	os.WriteFile(viejo, []byte("de antes"), 0o644)
	body := "capa privada"
	d := "sha256:" + shaHex([]byte(body))
	if rr := putOCIBlob(s, d, strings.NewReader(body), int64(len(body))); rr.Code != http.StatusCreated {
		t.Fatalf("PUT = %d %s", rr.Code, rr.Body)
	}
	filepath.Walk(filepath.Join(root, "cache", "oci"), func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is %o: only root may reach it", p, fi.Mode().Perm())
		}
		return nil
	})
	if fi, _ := os.Lstat(filepath.Join(dir, shaHex([]byte(body)))); fi.Mode().Perm() != 0o600 {
		t.Fatalf("blob %o, want 0600", fi.Mode().Perm())
	}
}

// sinLecturaParaOtros falla si algo bajo dir (incluido él) deja leer,
// escribir o atravesar a otros, o no es del grupo esperado (como root).
func sinLecturaParaOtros(t *testing.T, dir string) {
	t.Helper()
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o007 != 0 {
			t.Errorf("%s is %o: other accounts can reach it", p, fi.Mode().Perm())
		}
		return nil
	})
}

// Pasado el tope, una subida barre lo viejo de la caché de root; si aun así
// no cabe, 507. Lo de menos de graciaCacheOCI no se barre.
func TestPutOCIBlobTope(t *testing.T) {
	s, root := servidorBlobs(t)
	s.SetBuildCache(func() LimitesCacheConstruccion { return LimitesCacheConstruccion{MaxGiB: 1} })
	dir := filepath.Join(root, "cache", "oci", "sha256")
	os.MkdirAll(dir, 0o700)
	hace := time.Now().Add(-3 * graciaCacheOCI)
	grande := func(c byte, mod time.Time) string {
		p := filepath.Join(dir, strings.Repeat(string(c), 64))
		f, _ := os.Create(p)
		f.Truncate(600 << 20) // disperso: cuenta lo que dice medir
		f.Close()
		os.Chtimes(p, mod, mod)
		return p
	}
	viejo := grande('a', hace)
	body := "capa"
	d := "sha256:" + shaHex([]byte(body))
	if rr := putOCIBlob(s, d, strings.NewReader(body), 500<<20); rr.Code != http.StatusInsufficientStorage &&
		rr.Code != http.StatusBadRequest {
		t.Fatalf("PUT = %d %s", rr.Code, rr.Body)
	}
	if _, err := os.Lstat(viejo); !os.IsNotExist(err) {
		t.Fatal("over the limit, the old blob was not swept")
	}
	reciente := grande('c', time.Now())
	grande('d', time.Now())
	rr := putOCIBlob(s, d, strings.NewReader(body), int64(len(body)))
	if rr.Code != http.StatusInsufficientStorage || !strings.Contains(rr.Body.String(), "build_cache_max_gib") {
		t.Fatalf("PUT over the limit = %d %s", rr.Code, rr.Body)
	}
	if _, err := os.Lstat(reciente); err != nil {
		t.Fatal("a recent upload was swept")
	}
}

// Con constructores sin root, lo que pasó a la caché verificada cuenta como
// "ya está": reimportar el archivo no vuelve a subirlo.
func TestOCIBlobEnLaVerificada(t *testing.T) {
	s, root := servidorBlobs(t)
	body := "capa ya verificada"
	h := shaHex([]byte(body))
	os.MkdirAll(filepath.Join(root, "cache"), 0o755)
	v, err := prepararVerificada(root, yo())
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(v, "oci", "sha256", h)
	os.WriteFile(p, []byte(body), 0o640)
	viejo := time.Now().Add(-48 * time.Hour)
	os.Chtimes(p, viejo, viejo)
	if rr := getOCIBlob(s, "sha256:"+h); rr.Code != http.StatusNotFound {
		t.Fatalf("without a builder user, GET = %d", rr.Code)
	}
	s.constructor = yo()
	if rr := getOCIBlob(s, "sha256:"+h); rr.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", rr.Code, rr.Body)
	}
	if fi, _ := os.Lstat(p); time.Since(fi.ModTime()) > time.Hour {
		t.Fatal("GET did not touch the blob it is about to use")
	}
	if rr := putOCIBlob(s, "sha256:"+h, strings.NewReader(body), int64(len(body))); rr.Code != http.StatusOK ||
		!strings.Contains(rr.Body.String(), `"unchanged":true`) {
		t.Fatalf("PUT = %d %s", rr.Code, rr.Body)
	}
}

// Lo que pasó a la verificada de otro archivo también cuenta como "ya está"
// (enlazarArchivo lo toma de ahí); lo de un registro con credenciales no: no
// pasa a un archivo.
func TestOCIBlobEnLaVerificadaDeOtroArchivo(t *testing.T) {
	s, root := servidorBlobs(t)
	s.constructor = yo()
	os.MkdirAll(filepath.Join(root, "cache"), 0o755)
	v, err := prepararVerificada(root, yo())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		ambito string
		quiero int
	}{
		{ambitoArchivo("sha256:" + strings.Repeat("1", 64)), http.StatusOK},
		{ambitoRegistro("localhost:5000"), http.StatusNotFound},
	} {
		body := "capa de " + c.ambito
		h := shaHex([]byte(body))
		d, err := abrirAmbito(v, c.ambito, yo())
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(d, "oci", "sha256", h), []byte(body), 0o640)
		if rr := getOCIBlob(s, "sha256:"+h); rr.Code != c.quiero {
			t.Errorf("%s: GET = %d, quiero %d", c.ambito, rr.Code, c.quiero)
		}
	}
}
