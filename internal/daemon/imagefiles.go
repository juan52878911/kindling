package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

// maxImageFileUpload es lo máximo que se puede subir inline (content_b64) para
// meter en una imagen. Lo grande —un binario— va por from_host.
const maxImageFileUpload = 8 << 20

// libDir es el directorio de librerías del daemon: de ahí salen los ficheros
// from_host. Lo instala el administrador (make deploy), así que lo que haya es
// de confianza.
func libDir() string {
	if d := os.Getenv("KLING_LIB_DIR"); d != "" {
		return d
	}
	return "/usr/local/lib/kindling"
}

// resolveFromHost convierte un from_host relativo en una ruta dentro de libDir,
// sin dejar que salga de él.
func resolveFromHost(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.Contains(rel, "..") || strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("from_host must be a path relative to %s: %q", libDir(), rel)
	}
	p := filepath.Join(libDir(), filepath.Clean(rel))
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return "", fmt.Errorf("from_host %q is not a file in %s", rel, libDir())
	}
	return p, nil
}

func parseMode(s string) (os.FileMode, error) {
	if s == "" {
		return 0o644, nil
	}
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil || n > 0o777 {
		return 0, fmt.Errorf("mode must be octal permissions like 0644 or 0755 (no setuid): %q", s)
	}
	return os.FileMode(n), nil
}

func (s *Server) handleGetImageFile(w http.ResponseWriter, r *http.Request) {
	name, p := r.PathValue("name"), r.URL.Query().Get("path")
	if r.URL.Query().Get("stat") == "1" {
		st, err := s.mgr.StatImageFile(r.Context(), name, p)
		if err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
		return
	}
	max := int64(1 << 20)
	if v := r.URL.Query().Get("max"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 || n > 64<<20 {
			fail(w, http.StatusBadRequest, fmt.Errorf("max must be between 1 and %d", 64<<20))
			return
		}
		max = n
	}
	b, err := s.mgr.ReadImageFile(r.Context(), name, p, max)
	if errors.Is(err, machine.ErrNotInImage) {
		fail(w, http.StatusNotFound, fmt.Errorf("%s is not in image %s", p, name))
		return
	}
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(b)
}

func (s *Server) handlePutImageFile(w http.ResponseWriter, r *http.Request) {
	var req api.PutImageFileRequest
	body, err := api.LeerCuerpo(r.Body, maxImageFileUpload*2)
	if err != nil {
		fail(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	src, cleanup, code, err := s.imageFileSource(req)
	if err != nil {
		fail(w, code, err)
		return
	}
	defer cleanup()
	mode, err := parseMode(req.Mode)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	// Sin cancelación del cliente: se está escribiendo dentro de un ext4 montado.
	res, err := s.mgr.PutImageFile(context.WithoutCancel(r.Context()), r.PathValue("name"), req.Path, src, mode, req.Create)
	switch {
	case errors.Is(err, machine.ErrNotInImage):
		res.Error = fmt.Sprintf("%s is not in the image and create was not requested", req.Path)
		writeJSON(w, http.StatusOK, res)
	case res.Busy:
		res.Error = err.Error()
		writeJSON(w, http.StatusConflict, res)
	case err != nil:
		fail(w, http.StatusBadRequest, err)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

// imageFileSource deja el contenido pedido en un fichero del host y devuelve su
// ruta, con qué limpiarlo, y el código de error si no se puede.
func (s *Server) imageFileSource(req api.PutImageFileRequest) (string, func(), int, error) {
	nada := func() {}
	switch {
	case req.FromHost != "" && req.ContentB64 != "":
		return "", nada, http.StatusBadRequest, fmt.Errorf("use from_host or content_b64, not both")
	case req.FromHost != "":
		p, err := resolveFromHost(req.FromHost)
		if err != nil {
			return "", nada, http.StatusBadRequest, err
		}
		return p, nada, 0, nil
	case req.ContentB64 != "":
		b, err := base64.StdEncoding.DecodeString(req.ContentB64)
		if err != nil {
			return "", nada, http.StatusBadRequest, fmt.Errorf("content_b64: %v", err)
		}
		if len(b) > maxImageFileUpload {
			return "", nada, http.StatusRequestEntityTooLarge,
				fmt.Errorf("content is %d bytes; the limit is %d (use from_host for large files)", len(b), maxImageFileUpload)
		}
		dir := filepath.Join(s.root, "build")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", nada, http.StatusInternalServerError, err
		}
		f, err := os.CreateTemp(dir, "imagefile-")
		if err != nil {
			return "", nada, http.StatusInternalServerError, err
		}
		_, werr := f.Write(b)
		cerr := f.Close()
		if werr != nil || cerr != nil {
			os.Remove(f.Name())
			return "", nada, http.StatusInternalServerError, errors.Join(werr, cerr)
		}
		return f.Name(), func() { os.Remove(f.Name()) }, 0, nil
	default:
		return "", nada, http.StatusBadRequest, fmt.Errorf("missing from_host or content_b64")
	}
}
