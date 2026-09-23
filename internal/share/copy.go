package share

// La subida del modo copy: un tar que manda el cliente y que se extrae en un
// directorio privado del daemon para construir con él un ext4.
//
// El cliente no es de fiar por serlo. Hablar con el socket del daemon ya es
// mucho poder, pero un tar es un formato que se presta a trampas —rutas
// absolutas, "..", enlaces que apuntan fuera y entradas que los atraviesan,
// dispositivos—, y el que llega aquí puede venir de un CLI viejo, de un script
// o de un sandbox que alguien automatizó. Cualquier entrada rara rechaza la
// subida ENTERA: una copia a medias sería peor que ninguna.

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	proto "github.com/juan52878911/kindling/pkg/share"
)

// MaxCopyEntries acota el número de entradas de una subida: cada una es un
// inodo del ext4 y una llamada al sistema al extraer.
const MaxCopyEntries = 1 << 20

// CopyStats resume una subida extraída.
type CopyStats struct {
	Bytes    int64 // contenido de los ficheros regulares
	Files    int   // ficheros regulares
	Dirs     int
	Symlinks int
	// Blocks estima los bloques de 4 KiB que ocupará en un ext4.
	Blocks int64
}

// Entries es el total de entradas.
func (s CopyStats) Entries() int { return s.Files + s.Dirs + s.Symlinks }

// ErrTooLarge es el error de una subida que pasa del tope.
var ErrTooLarge = errors.New("upload too large")

// Extract lee un tar de r y lo extrae en dst, que tiene que existir y estar
// vacío. maxBytes acota el contenido de los ficheros regulares.
func Extract(r io.Reader, dst string, maxBytes int64) (CopyStats, error) {
	var st CopyStats
	root, err := os.OpenRoot(dst)
	if err != nil {
		return st, err
	}
	defer root.Close()

	tr := tar.NewReader(r)
	seen := map[string]bool{".": true}
	dirs := map[string]bool{".": true}
	links := map[string]string{}

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return st, checkLinks(links)
		}
		if err != nil {
			return st, fmt.Errorf("reading the archive: %w", err)
		}
		if st.Entries() >= MaxCopyEntries {
			return st, fmt.Errorf("more than %d entries", MaxCopyEntries)
		}
		name, err := entryName(h.Name)
		if err != nil {
			return st, err
		}
		if name == "." {
			// La raíz del propio tar ("./"): ya existe.
			if h.Typeflag != tar.TypeDir {
				return st, errors.New("the archive root is not a directory")
			}
			continue
		}
		if seen[name] {
			return st, fmt.Errorf("%q appears twice in the archive", name)
		}
		seen[name] = true

		// Nada puede colgar de un enlace: escribir a través de él haría que
		// la comprobación de destino de los enlaces (que es léxica) no valga.
		for p := path.Dir(name); p != "."; p = path.Dir(p) {
			if _, ok := links[p]; ok {
				return st, fmt.Errorf("%q is inside the symlink %q", name, p)
			}
		}
		if err := mkParents(root, dirs, name); err != nil {
			return st, err
		}

		mode := fs.FileMode(h.Mode) & 0o777
		switch h.Typeflag {
		case tar.TypeDir:
			// Pudo nacer antes como padre implícito de otra entrada.
			if !dirs[name] {
				if err := root.Mkdir(name, mode|0o700); err != nil {
					return st, fmt.Errorf("creating %s: %w", name, err)
				}
			}
			dirs[name] = true
			st.Dirs++
			st.Blocks++

		case tar.TypeReg, tar.TypeRegA:
			if h.Size < 0 {
				return st, fmt.Errorf("%q has a negative size", name)
			}
			// Restando y no sumando: un tamaño enorme en la cabecera no puede
			// desbordar la cuenta.
			if h.Size > maxBytes-st.Bytes {
				return st, fmt.Errorf("%w: more than %d MiB (daemon.share_copy_max_mib)", ErrTooLarge, maxBytes>>20)
			}
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return st, fmt.Errorf("creating %s: %w", name, err)
			}
			// El modo después y sin umask, para que el invitado vea el que
			// traía el fichero. Con lectura para el dueño siempre: mke2fs lo
			// tiene que leer, y en macOS el daemon no es root.
			_ = f.Chmod(mode | 0o400)
			n, err := io.Copy(f, io.LimitReader(tr, h.Size))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return st, fmt.Errorf("writing %s: %w", name, err)
			}
			if n != h.Size {
				return st, fmt.Errorf("%s is truncated in the archive", name)
			}
			st.Bytes += n
			st.Files++
			st.Blocks += (n + 4095) / 4096

		case tar.TypeSymlink:
			if err := proto.CheckLink(name, h.Linkname); err != nil {
				return st, err
			}
			// El padre es un directorio que creamos nosotros (ninguno es un
			// enlace, comprobado arriba), así que la ruta del host no se desvía.
			if err := os.Symlink(h.Linkname, filepath.Join(dst, filepath.FromSlash(name))); err != nil {
				return st, fmt.Errorf("creating symlink %s: %w", name, err)
			}
			links[name] = h.Linkname
			st.Symlinks++
			st.Blocks++

		case tar.TypeLink:
			return st, fmt.Errorf("%q is a hard link; send it as a regular file", name)
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			return st, fmt.Errorf("%q is a device or a FIFO: not allowed in a share", name)
		default:
			return st, fmt.Errorf("%q has an unsupported entry type %q", name, h.Typeflag)
		}
	}
}

// entryName valida y normaliza el nombre de una entrada.
func entryName(raw string) (string, error) {
	if raw == "" || len(raw) > proto.MaxPath || strings.ContainsRune(raw, 0) {
		return "", fmt.Errorf("invalid entry name %q", raw)
	}
	if strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("%q is an absolute path", raw)
	}
	for _, c := range strings.Split(strings.TrimSuffix(raw, "/"), "/") {
		if c == ".." {
			return "", fmt.Errorf("%q contains ..", raw)
		}
		if len(c) > proto.MaxName {
			return "", fmt.Errorf("%q has a component longer than %d bytes", raw, proto.MaxName)
		}
	}
	return path.Clean(raw), nil
}

// checkLinks repasa los enlaces cuando ya se conocen todos: un destino puede
// atravesar otro enlace del mismo árbol, y entonces la comprobación léxica de
// CheckLink no dice nada de adónde llega de verdad.
func checkLinks(links map[string]string) error {
	isLink := func(p string) bool { _, ok := links[p]; return ok }
	for name, target := range links {
		if proto.LinkTraverses(name, target, isLink) {
			return fmt.Errorf("symlink %q goes through another symlink (%s)", name, target)
		}
	}
	return nil
}

// mkParents crea los directorios que falten por encima de name: un tar puede
// traer "a/b/c" sin haber traído antes "a/".
func mkParents(root *os.Root, dirs map[string]bool, name string) error {
	var todo []string
	for p := path.Dir(name); !dirs[p]; p = path.Dir(p) {
		todo = append(todo, p)
	}
	for i := len(todo) - 1; i >= 0; i-- {
		if err := root.Mkdir(todo[i], 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", todo[i], err)
		}
		dirs[todo[i]] = true
	}
	return nil
}
