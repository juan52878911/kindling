package daemon

// Blobs OCI subidos por el CLI: GET y PUT /oci/blobs/{digest}.
//
// `kling image import -archive` lee un docker save o un layout OCI en la
// máquina del CLI, que puede no ser la del daemon (el API va por un socket,
// local o por SSH). Sube cada blob que falte —el manifiesto, la configuración
// y las capas— en flujo, y el daemon lo deja en su caché de blobs OCI,
// $ROOT/cache/oci/sha256/<hex>, la misma en la que el constructor oci deja lo
// que baja de un registro. Después el constructor construye desde ahí sin red
// (oci.Client.Offline), y comprueba la cadena manifiesto → configuración →
// capas → diff_ids como con un registro.
//
// SUPERFICIE: la ruta sale solo del digest (sha256:<64 hex>, lista blanca); el
// cuerpo tiene que traer Content-Length, acotado (api.MaxBlobBytes), y se
// escribe en un temporal que solo se renombra cuando su sha256 es el del
// digest: lo que hay en la caché con ese nombre es siempre ese contenido, lo
// suba quien lo suba. Nada se ejecuta ni se interpreta aquí.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/lazyre"
)

var reOCIDigest = lazyre.New(`^sha256:[0-9a-f]{64}$`)

// ociBlobPath es dónde queda el blob digest en la caché.
func (s *Server) ociBlobPath(digest string) (string, error) {
	if !reOCIDigest.MatchString(digest) {
		return "", fmt.Errorf("invalid digest %q (want sha256:<64 hex>)", digest)
	}
	return filepath.Join(s.root, "cache", "oci", "sha256", strings.TrimPrefix(digest, "sha256:")), nil
}

// ociBlobPresente dice si el blob ya está, con ese tamaño: un fichero regular
// (sin seguir enlaces) que solo llegó ahí comprobado (handlePutOCIBlob, o la
// descarga del constructor que corre como root).
func ociBlobPresente(p string) (int64, bool) {
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o022 != 0 {
		return 0, false
	}
	return fi.Size(), true
}

// handleGetOCIBlob dice si el daemon ya tiene un blob (no lo sirve: es para
// no volver a subirlo). 404 si no.
func (s *Server) handleGetOCIBlob(w http.ResponseWriter, r *http.Request) {
	digest := r.PathValue("digest")
	p, err := s.ociBlobPath(digest)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	n, ok := ociBlobPresente(p)
	if !ok {
		fail(w, http.StatusNotFound, fmt.Errorf("blob %s is not in the cache", digest))
		return
	}
	writeJSON(w, http.StatusOK, api.OCIBlob{Digest: digest, Size: n})
}

// handlePutOCIBlob recibe un blob en flujo y lo deja en la caché si su sha256
// es el del digest.
func (s *Server) handlePutOCIBlob(w http.ResponseWriter, r *http.Request) {
	digest := r.PathValue("digest")
	p, err := s.ociBlobPath(digest)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	size := r.ContentLength
	if size < 0 {
		fail(w, http.StatusLengthRequired, fmt.Errorf("a blob upload needs Content-Length"))
		return
	}
	if size > api.MaxBlobBytes {
		fail(w, http.StatusRequestEntityTooLarge, fmt.Errorf("the blob is %d bytes and the limit is %d", size, api.MaxBlobBytes))
		return
	}
	if n, ok := ociBlobPresente(p); ok && n == size {
		// Ya está (otra subida, o una descarga de un registro): no se lee nada.
		writeJSON(w, http.StatusOK, api.OCIBlob{Digest: digest, Size: n, Unchanged: true})
		return
	}
	// Sin el plazo de lectura de 30 s del servidor: gigas por SSH no caben, y
	// el tamaño ya está acotado.
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	// Un temporal propio por subida (dos a la vez del mismo blob no se pisan),
	// en el mismo directorio para que el renombrado sea atómico, y acabado en
	// .part como los de las descargas: nadie lo toma por un blob.
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	tmp := p + "." + hex.EncodeToString(rnd[:]) + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	limpiar := func() { f.Close(); _ = os.Remove(tmp) }
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r.Body, size+1))
	if err != nil {
		limpiar()
		fail(w, http.StatusBadRequest, fmt.Errorf("receiving blob %s: %w", digest, err))
		return
	}
	if n != size {
		limpiar()
		fail(w, http.StatusBadRequest, fmt.Errorf("blob %s: got %d bytes, Content-Length says %d", digest, n, size))
		return
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != digest {
		limpiar()
		fail(w, http.StatusBadRequest, fmt.Errorf("blob %s: sha256 mismatch (the content is %s)", digest, got))
		return
	}
	// Durable antes de renombrar: un corte no deja el nombre con medio blob.
	if err := f.Sync(); err != nil {
		limpiar()
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		fail(w, http.StatusInternalServerError, err)
		return
	}
	// Sin la máscara del daemon de por medio: 0644, como los que se bajan.
	if err := os.Chmod(tmp, 0o644); err != nil {
		_ = os.Remove(tmp)
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, api.OCIBlob{Digest: digest, Size: n})
}
