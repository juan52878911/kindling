package guest

// Ficheros dentro de la microVM: subir código, bajar resultados.
//
// Solo con kling.exec=1, igual que /exec: quien puede ejecutar puede escribir
// ficheros de todos modos, y quien no puede ejecutar no debe poder escribirlos
// (un fichero bien puesto es una forma de ejecutar).
//
//	GET    /files?path=/abs/ruta          el contenido, en crudo
//	GET    /files?path=/abs/ruta&stat=1   api.FileStat en JSON
//	PUT    /files?path=...&mode=0644[&mkdir=1]   escribe el cuerpo
//	DELETE /files?path=...                 borra un fichero o un directorio vacío
//
// El último componente de la ruta no se sigue si es un enlace (O_NOFOLLOW): el
// invitado es de usar y tirar y es root, así que no protege nada DENTRO, pero
// evita que un enlace plantado convierta "escribe en /tmp/x" en "escribe en
// otro sitio" sin que quien llama lo sepa.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/juan52878911/kindling/pkg/api"
)

// FilesHandler sirve /files.
func FilesHandler() http.HandlerFunc {
	return handleFiles
}

func handleFiles(w http.ResponseWriter, r *http.Request) {
	p, err := cleanAbs(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if r.URL.Query().Get("stat") == "1" {
			fi, err := os.Lstat(p)
			if err != nil {
				fileError(w, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(statOf(p, fi))
			return
		}
		f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			fileError(w, err)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			fileError(w, err)
			return
		}
		if fi.IsDir() {
			http.Error(w, p+" is a directory", http.StatusBadRequest)
			return
		}
		if fi.Size() > api.FileMaxDownload {
			http.Error(w, fmt.Sprintf("%s is %d bytes; the download limit is %d", p, fi.Size(), api.FileMaxDownload),
				http.StatusRequestEntityTooLarge)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
		_, _ = io.Copy(w, io.LimitReader(f, api.FileMaxDownload))

	case http.MethodPut:
		mode := os.FileMode(0o644)
		if s := r.URL.Query().Get("mode"); s != "" {
			n, err := strconv.ParseUint(s, 8, 32)
			if err != nil || n > 0o7777 {
				http.Error(w, fmt.Sprintf("invalid mode %q (octal, e.g. 0755)", s), http.StatusBadRequest)
				return
			}
			mode = os.FileMode(n)
		}
		if r.URL.Query().Get("mkdir") == "1" {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				fileError(w, err)
				return
			}
		}
		st, err := writeFile(p, mode, r.Body)
		if err != nil {
			fileError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)

	case http.MethodDelete:
		if err := os.Remove(p); err != nil {
			fileError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "use GET, PUT or DELETE", http.StatusMethodNotAllowed)
	}
}

// writeFile escribe al lado y renombra: quien lea el fichero ve el viejo o el
// nuevo, nunca uno a medias. Se pasa de FileMaxUpload con error, sin dejar nada.
func writeFile(p string, mode os.FileMode, body io.Reader) (*api.FileStat, error) {
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symbolic link; refusing to write through it", p)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".kling-put-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(body, api.FileMaxUpload+1))
	if err == nil && n > api.FileMaxUpload {
		err = errTooLarge
	}
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	st := statOf(p, fi)
	return &st, nil
}

var errTooLarge = fmt.Errorf("upload is over the %d byte limit", api.FileMaxUpload)

func statOf(p string, fi os.FileInfo) api.FileStat {
	return api.FileStat{
		Path: p, Size: fi.Size(), IsDir: fi.IsDir(), ModTime: fi.ModTime(),
		Mode: fmt.Sprintf("%04o", fi.Mode().Perm()|fi.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)),
	}
}

// cleanAbs exige una ruta absoluta y la normaliza. "/" no vale para escribir ni
// borrar, pero eso lo dicen las llamadas al sistema.
func cleanAbs(p string) (string, error) {
	if p == "" {
		return "", errors.New("missing path")
	}
	if !path.IsAbs(p) {
		return "", fmt.Errorf("path must be absolute: %q", p)
	}
	return path.Clean(p), nil
}

func fileError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errTooLarge):
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
	case errors.Is(err, syscall.ELOOP):
		http.Error(w, "the last path component is a symbolic link; not following it", http.StatusBadRequest)
	case errors.Is(err, fs.ErrPermission):
		http.Error(w, err.Error(), http.StatusForbidden)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}
