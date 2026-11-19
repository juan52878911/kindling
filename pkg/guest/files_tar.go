package guest

// Directorios dentro de la microVM: listado y tar.
//
//	GET /files?path=/dir&list=1            api.FileList en JSON
//	GET /files?path=/dir&format=tar        el árbol como tar
//	PUT /files?path=/dir&format=tar[&mkdir=1]  extrae el tar dentro de /dir
//
// El tar que llega no es de fiar: se rechaza TODO el archivo (lo ya escrito
// se queda, no hay forma de deshacerlo sin copiar dos veces) ante una entrada
// con "..", absoluta, un enlace que sale del directorio, un dispositivo, o si
// pasa de los topes de tamaño o de entradas. La extracción se hace dentro de
// un os.Root, de modo que ni un enlace plantado de antes puede sacarla.

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/api"

	proto "github.com/juan52878911/kindling/internal/shareproto"
)

// listDir sirve ?list=1.
func listDir(w http.ResponseWriter, p string) {
	fi, err := os.Lstat(p)
	if err != nil {
		fileError(w, err)
		return
	}
	if !fi.IsDir() {
		http.Error(w, p+" is not a directory", http.StatusBadRequest)
		return
	}
	d, err := os.Open(p)
	if err != nil {
		fileError(w, err)
		return
	}
	defer d.Close()
	out := api.FileList{Path: p, Entries: []api.FileEntry{}}
	// Se leen max+1 entradas para saber si hay más; ReadDir no ordena, así
	// que con truncado el corte es arbitrario pero el resultado va ordenado.
	des, err := d.ReadDir(api.FileListMax + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		fileError(w, err)
		return
	}
	if len(des) > api.FileListMax {
		des = des[:api.FileListMax]
		out.Truncated = true
	}
	for _, de := range des {
		info, err := de.Info()
		if err != nil {
			continue // desapareció entre el listado y el stat
		}
		e := api.FileEntry{
			Name: de.Name(), Type: entryType(info.Mode()), Size: info.Size(),
			Mode: modeString(info), ModTime: info.ModTime(),
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			e.Target, _ = os.Readlink(filepath.Join(p, de.Name()))
		}
		out.Entries = append(out.Entries, e)
	}
	sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Name < out.Entries[j].Name })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func entryType(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return "dir"
	case m&fs.ModeSymlink != 0:
		return "symlink"
	case m.IsRegular():
		return "file"
	default:
		return "other"
	}
}

func modeString(fi os.FileInfo) string {
	return fmt.Sprintf("%04o", fi.Mode().Perm()|fi.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky))
}

// serveTar sirve ?format=tar: el contenido de p (sin el propio directorio)
// como tar. Antes de empezar a emitir recorre el árbol para comprobar los
// topes, porque con la respuesta ya empezada ya no se puede dar un 413.
func serveTar(w http.ResponseWriter, p string) {
	fi, err := os.Lstat(p)
	if err != nil {
		fileError(w, err)
		return
	}
	if !fi.IsDir() {
		http.Error(w, p+" is not a directory", http.StatusBadRequest)
		return
	}
	var bytes int64
	var entries int
	err = filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entries++
		if entries > api.FileTarMaxEntries {
			return errTarEntries
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			bytes += info.Size()
			if bytes > api.FileMaxDownload {
				return errTarTooBig
			}
		}
		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, errTarTooBig), errors.Is(err, errTarEntries):
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		default:
			fileError(w, err)
		}
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	tw := tar.NewWriter(w)
	_ = filepath.WalkDir(p, func(full string, d fs.DirEntry, err error) error {
		if err != nil || full == p {
			return nil // ya empezó la respuesta: lo que no se puede leer se salta
		}
		rel, err := filepath.Rel(p, full)
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		var link string
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			link, _ = os.Readlink(full)
		case !info.Mode().IsRegular() && !info.IsDir():
			return nil // FIFO, sockets, dispositivos: no viajan
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return nil
		}
		h.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			h.Name += "/"
		}
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if tw.WriteHeader(h) != nil {
			return fs.SkipAll
		}
		if info.Mode().IsRegular() {
			f, err := os.OpenFile(full, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
			if err != nil {
				// La cabecera ya salió con su tamaño: hay que rellenar.
				_, _ = io.CopyN(tw, zeroReader{}, h.Size)
				return nil
			}
			n, _ := io.Copy(tw, io.LimitReader(f, h.Size))
			f.Close()
			if n < h.Size { // el fichero encogió mientras se leía
				_, _ = io.CopyN(tw, zeroReader{}, h.Size-n)
			}
		}
		return nil
	})
	_ = tw.Close()
}

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }

var (
	errTarTooBig  = fmt.Errorf("the tree is over the %d byte download limit", api.FileMaxDownload)
	errTarEntries = fmt.Errorf("the tree has more than %d entries", api.FileTarMaxEntries)
)

// putTar sirve PUT ?format=tar.
func putTar(w http.ResponseWriter, r *http.Request, p string) {
	if r.URL.Query().Get("mkdir") == "1" {
		if err := os.MkdirAll(p, 0o755); err != nil {
			fileError(w, err)
			return
		}
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(15 * time.Minute))
	st, err := ExtractTar(r.Body, p, api.FileMaxUpload, api.FileTarMaxEntries)
	if err != nil {
		var re *tarRejected
		if errors.As(err, &re) {
			code := http.StatusBadRequest
			if re.tooLarge {
				code = http.StatusRequestEntityTooLarge
			}
			http.Error(w, err.Error(), code)
			return
		}
		fileError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

type tarRejected struct {
	msg      string
	tooLarge bool
}

func (e *tarRejected) Error() string { return e.msg }

func reject(format string, a ...any) error { return &tarRejected{msg: fmt.Sprintf(format, a...)} }

// ExtractTar extrae un tar en dst (que tiene que existir) sin salir de él.
// maxBytes acota el contenido de los ficheros y maxEntries el número de
// entradas; el propio tar también se lee con tope.
func ExtractTar(r io.Reader, dst string, maxBytes int64, maxEntries int) (api.TarResult, error) {
	var res api.TarResult
	fi, err := os.Stat(dst)
	if err != nil {
		return res, err
	}
	if !fi.IsDir() {
		return res, reject("%s is not a directory", dst)
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		return res, err
	}
	defer root.Close()

	// Una bomba de cabeceras la acota maxEntries; el flujo total, el tope de
	// bytes más 1 KiB por entrada.
	tr := tar.NewReader(io.LimitReader(r, maxBytes+int64(maxEntries)*1024))
	seen := map[string]bool{}
	links := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return res, reject("reading the archive: %v", err)
		}
		if res.Entries >= maxEntries {
			return res, &tarRejected{msg: fmt.Sprintf("more than %d entries", maxEntries), tooLarge: true}
		}
		name, err := tarEntryName(h.Name)
		if err != nil {
			return res, err
		}
		if name == "." {
			if h.Typeflag != tar.TypeDir {
				return res, reject("the archive root is not a directory")
			}
			continue
		}
		if seen[name] {
			return res, reject("%q appears twice in the archive", name)
		}
		seen[name] = true
		for q := path.Dir(name); q != "."; q = path.Dir(q) {
			if _, ok := links[q]; ok {
				return res, reject("%q is inside the symlink %q", name, q)
			}
		}
		if parent := path.Dir(name); parent != "." {
			if err := mkParents(root, parent); err != nil {
				return res, err
			}
		}
		mode := fs.FileMode(h.Mode) & 0o777
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.Mkdir(name, mode|0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return res, fmt.Errorf("creating %s: %w", name, err)
			}
			res.Dirs++
		//lint:ignore SA1019 los tar antiguos marcan los ficheros con TypeRegA
		case tar.TypeReg, tar.TypeRegA:
			if h.Size < 0 {
				return res, reject("%q has a negative size", name)
			}
			if h.Size > maxBytes-res.Bytes {
				return res, &tarRejected{msg: fmt.Sprintf("the archive holds more than %d bytes", maxBytes), tooLarge: true}
			}
			if err := removeIfNotDir(root, name); err != nil {
				return res, err
			}
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return res, fmt.Errorf("creating %s: %w", name, err)
			}
			n, err := io.Copy(f, io.LimitReader(tr, h.Size))
			if err == nil {
				err = f.Chmod(mode)
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return res, fmt.Errorf("writing %s: %w", name, err)
			}
			if n != h.Size {
				return res, reject("%s is truncated in the archive", name)
			}
			res.Bytes += n
			res.Files++
		case tar.TypeSymlink:
			if err := proto.CheckLink(name, h.Linkname); err != nil {
				return res, reject("%v", err)
			}
			if err := removeIfNotDir(root, name); err != nil {
				return res, err
			}
			// Los padres son directorios de verdad (mkParents), así que la
			// ruta de fuera de la raíz no se desvía.
			if err := os.Symlink(h.Linkname, filepath.Join(dst, filepath.FromSlash(name))); err != nil {
				return res, fmt.Errorf("creating symlink %s: %w", name, err)
			}
			links[name] = h.Linkname
			res.Symlinks++
		case tar.TypeLink:
			return res, reject("%q is a hard link; send it as a regular file", name)
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			return res, reject("%q is a device or a FIFO: not allowed", name)
		default:
			return res, reject("%q has an unsupported entry type %q", name, h.Typeflag)
		}
		res.Entries++
	}
	isLink := func(q string) bool { _, ok := links[q]; return ok }
	for name, target := range links {
		if proto.LinkTraverses(name, target, isLink) {
			return res, reject("symlink %q goes through another symlink (%s)", name, target)
		}
	}
	return res, nil
}

// removeIfNotDir quita lo que hubiera en name para sustituirlo, sin seguir
// enlaces. Un directorio no se sustituye por un fichero.
func removeIfNotDir(root *os.Root, name string) error {
	fi, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return reject("%q is a directory in the machine; the archive has a file there", name)
	}
	return root.Remove(name)
}

func tarEntryName(raw string) (string, error) {
	if raw == "" || len(raw) > proto.MaxPath || strings.ContainsRune(raw, 0) {
		return "", reject("invalid entry name %q", raw)
	}
	if strings.HasPrefix(raw, "/") {
		return "", reject("%q is an absolute path", raw)
	}
	for _, c := range strings.Split(strings.TrimSuffix(raw, "/"), "/") {
		if c == ".." {
			return "", reject("%q contains ..", raw)
		}
		if len(c) > proto.MaxName {
			return "", reject("%q has a component longer than %d bytes", raw, proto.MaxName)
		}
	}
	return path.Clean(raw), nil
}

// mkParents crea los directorios que falten de p dentro de root y rechaza que
// alguno sea un enlace (uno plantado antes en la máquina llevaría la
// extracción adonde no se mira).
func mkParents(root *os.Root, p string) error {
	cur := ""
	for _, c := range strings.Split(p, "/") {
		cur = path.Join(cur, c)
		fi, err := root.Lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := root.Mkdir(cur, 0o755); err != nil {
				return fmt.Errorf("creating %s: %w", cur, err)
			}
		case err != nil:
			return err
		case !fi.IsDir():
			return reject("%q is not a directory in the machine (symlink or file)", cur)
		}
	}
	return nil
}
