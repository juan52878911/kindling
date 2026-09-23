package daemon

// Transferencia de imágenes entre daemons: GET y PUT /images/{name}/blob.
//
// Existe para macOS, donde no se construyen imágenes: se construyen en un host
// Linux y `kling images copy` las mueve de un daemon a otro, en flujo, sin
// pasar por el disco del CLI. Una imagen son hasta tres ficheros —el ext4 (o
// la capa), la receta y, compartido, el kernel—, y cada uno viaja por separado
// con su parte en ?part=.
//
// SUPERFICIE: el PUT escribe en $ROOT/images, así que el nombre y la parte se
// validan con lista blanca (el nombre acaba en una ruta), el cuerpo se acota
// (api.MaxBlobBytes) y se escribe en un temporal que solo se renombra cuando
// está entero y, si el cliente dio su sha256, verificado. Nunca se sustituye
// una imagen en uso por un contenido distinto.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// blobTarget es el fichero de una parte de una imagen.
type blobTarget struct {
	name, part, path string
}

// resolveBlob traduce (nombre, parte) a su fichero. Con part vacía y para
// leer, elige la que exista: el ext4 de una monolítica o la capa de una por
// capas. Para escribir, la parte es obligatoria.
func (s *Server) resolveBlob(name, part string, write bool) (blobTarget, error) {
	dir := filepath.Join(s.root, "images")
	if name == api.KernelBlobName {
		if part != "" && part != api.BlobKernel {
			return blobTarget{}, fmt.Errorf("%q is the kernel: its only part is %q", name, api.BlobKernel)
		}
		return blobTarget{name, api.BlobKernel, filepath.Join(dir, "vmlinux")}, nil
	}
	if !reName.MatchString(name) {
		return blobTarget{}, fmt.Errorf("invalid image name %q", name)
	}
	switch part {
	case api.BlobImage:
		return blobTarget{name, part, filepath.Join(dir, name+".ext4")}, nil
	case api.BlobLayer:
		return blobTarget{name, part, filepath.Join(dir, name+".layer.ext4")}, nil
	case api.BlobRecipe:
		return blobTarget{name, part, s.recipePath(name)}, nil
	case "":
		if write {
			return blobTarget{}, fmt.Errorf("missing ?part= (%s, %s or %s)", api.BlobImage, api.BlobLayer, api.BlobRecipe)
		}
		for _, p := range []string{api.BlobImage, api.BlobLayer} {
			t, _ := s.resolveBlob(name, p, false)
			if _, err := os.Stat(t.path); err == nil {
				return t, nil
			}
		}
		return blobTarget{}, &api.StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("image %q does not exist", name)}
	default:
		return blobTarget{}, fmt.Errorf("unknown part %q: use %s, %s or %s", part, api.BlobImage, api.BlobLayer, api.BlobRecipe)
	}
}

// sha256File calcula el sha256 de un fichero en hexadecimal.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// handleGetImageBlob sirve una parte de una imagen, con su sha256 en la
// cabecera para que el destino verifique lo que recibe. También contesta a
// HEAD (el patrón GET del mux lo cubre), que es como el CLI pregunta si el
// destino ya tiene lo mismo antes de mandar gigas.
func (s *Server) handleGetImageBlob(w http.ResponseWriter, r *http.Request) {
	t, err := s.resolveBlob(r.PathValue("name"), r.URL.Query().Get("part"), false)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	f, err := os.Open(t.path)
	if err != nil {
		fail(w, http.StatusNotFound, fmt.Errorf("%s of %q does not exist", t.part, t.name))
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		fail(w, http.StatusNotFound, fmt.Errorf("%s of %q is not a regular file", t.part, t.name))
		return
	}
	// El hash se calcula entero ANTES de mandar nada: va en una cabecera, y las
	// cabeceras salen antes que el cuerpo. Leer dos veces cuesta, pero la
	// alternativa —un trailer— no la ven los clientes HTTP corrientes.
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.Header().Set(api.HeaderSha256, hex.EncodeToString(h.Sum(nil)))
	w.Header().Set(api.HeaderBlobPart, t.part)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	// Sin plazo de escritura: una imagen de gigas por SSH tarda lo que tarda.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	_, _ = io.Copy(w, io.LimitReader(f, fi.Size()))
}

// handlePutImageBlob recibe una parte de una imagen en flujo.
func (s *Server) handlePutImageBlob(w http.ResponseWriter, r *http.Request) {
	t, err := s.resolveBlob(r.PathValue("name"), r.URL.Query().Get("part"), true)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if r.ContentLength > api.MaxBlobBytes {
		fail(w, http.StatusRequestEntityTooLarge, fmt.Errorf("the %s is %d bytes and the limit is %d", t.part, r.ContentLength, api.MaxBlobBytes))
		return
	}
	want := r.Header.Get(api.HeaderSha256)
	if want != "" {
		if b, err := hex.DecodeString(want); err != nil || len(b) != sha256.Size {
			fail(w, http.StatusBadRequest, fmt.Errorf("%s must be a hex sha256", api.HeaderSha256))
			return
		}
	}
	// El servidor corta cada petición a los 30 s de lectura (ver Listen). Aquí
	// no: gigas por SSH no caben en 30 s, y el tamaño ya está acotado arriba.
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})

	if err := os.MkdirAll(filepath.Dir(t.path), 0o755); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	// Temporal en el MISMO directorio, para que el renombrado final sea
	// atómico: o está el fichero viejo o el nuevo entero, nunca medio.
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(filepath.Dir(t.path), "."+filepath.Base(t.path)+".tmp-"+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	limpiar := func() { f.Close(); _ = os.Remove(tmp) }

	h := sha256.New()
	// +1 para distinguir "justo el tope" de "se pasó" sin leer lo que sobra.
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r.Body, api.MaxBlobBytes+1))
	if err != nil {
		limpiar()
		fail(w, http.StatusBadRequest, fmt.Errorf("receiving the %s: %w", t.part, err))
		return
	}
	if n > api.MaxBlobBytes {
		limpiar()
		fail(w, http.StatusRequestEntityTooLarge, fmt.Errorf("the %s exceeds %d bytes", t.part, api.MaxBlobBytes))
		return
	}
	if r.ContentLength >= 0 && n != r.ContentLength {
		limpiar()
		fail(w, http.StatusBadRequest, fmt.Errorf("the %s arrived truncated: %d of %d bytes", t.part, n, r.ContentLength))
		return
	}
	got := hex.EncodeToString(h.Sum(nil))
	if want != "" && got != want {
		limpiar()
		fail(w, http.StatusBadRequest, fmt.Errorf("the %s arrived corrupted: sha256 %s, expected %s", t.part, got, want))
		return
	}
	// Durable antes de renombrar: un corte justo después no puede dejar una
	// imagen con el nombre bueno y el contenido a medias.
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

	res := api.BlobPutResult{Name: t.name, Part: t.part, Size: n, Sha256: got}
	if prev, err := sha256File(t.path); err == nil {
		if prev == got {
			// Idéntico: no se toca, y así no importa que esté en uso.
			_ = os.Remove(tmp)
			res.Unchanged = true
			writeJSON(w, http.StatusOK, res)
			return
		}
		if err := s.blobReplaceable(t); err != nil {
			_ = os.Remove(tmp)
			fail(w, http.StatusConflict, err)
			return
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tmp)
		fail(w, http.StatusInternalServerError, err)
		return
	} else if t.part == api.BlobRecipe && s.esCapa(t.name) {
		// Una capa sin receta va sobre la base por defecto: ponérsela ahora
		// puede cambiarle la base, así que cuenta como sustituir.
		if err := s.blobReplaceable(t); err != nil {
			_ = os.Remove(tmp)
			fail(w, http.StatusConflict, err)
			return
		}
	}
	if err := os.Rename(tmp, t.path); err != nil {
		_ = os.Remove(tmp)
		fail(w, http.StatusInternalServerError, err)
		return
	}
	// En Linux el VMM corre sin privilegios y tiene que poder leer la imagen.
	if t.part != api.BlobRecipe && t.part != api.BlobKernel {
		s.mgr.EnsureImageReadable(t.name)
	}
	writeJSON(w, http.StatusCreated, res)
}

// blobReplaceable dice si la parte t se puede sustituir por un contenido
// distinto. La receta cuenta como la imagen: en una por capas decide la base.
func (s *Server) blobReplaceable(t blobTarget) error {
	if t.part == api.BlobKernel {
		return s.mgr.KernelReplaceable()
	}
	return s.mgr.ImageReplaceable(t.name)
}

// esCapa dice si name es una imagen por capas ya presente.
func (s *Server) esCapa(name string) bool {
	_, err := os.Stat(filepath.Join(s.root, "images", name+".layer.ext4"))
	return err == nil
}
