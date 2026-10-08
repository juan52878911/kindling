package ext4

import (
	"archive/tar"
	"bufio"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

// TarOptions dice cómo se mete un tar en el árbol.
type TarOptions struct {
	// Prefix es el directorio del árbol donde cae la raíz del tar ("/android").
	Prefix string
	// Stream es el índice del flujo que traerá los datos al escribir.
	Stream int
	// Whiteouts aplica los ".wh." de una capa OCI sobre lo que ya hay.
	Whiteouts bool
	// Keep dice qué ficheros se leen ya a memoria en vez de esperar al flujo
	// (los que luego se editan). La ruta es la del árbol.
	Keep func(p string, size int64) bool
	// Time es la hora de los directorios que el tar no trae.
	Time time.Time
	// OnEntry recibe la ruta (relativa a Prefix, con "/" delante) de cada
	// entrada que se mete (la lista de ficheros de un paquete .deb).
	OnEntry func(rel string, h *tar.Header)
	// Entries, si no es nil, cuenta las entradas leídas (se comparte entre
	// varias llamadas: las capas de una imagen) y MaxEntries es su tope: un
	// tar con millones de entradas falla al pasarlo, antes de llenar la
	// memoria de nodos. 0 = sin tope.
	Entries    *int64
	MaxEntries int64
}

const maxKeep = 16 << 20

// AddTar recorre un tar y lo refleja en el árbol: estructura, dueños,
// modos, horas y xattrs (SCHILY.xattr.*). Los datos de los ficheros no se
// copian: quedan como StreamKey{Stream, número de entrada} para TarStream.
func (root *Node) AddTar(r io.Reader, o TarOptions) error {
	base, err := root.MkdirAll(o.Prefix, 0o755, 0, 0, o.Time)
	if err != nil {
		return err
	}
	baseDir := path.Clean("/" + o.Prefix)
	// Lo que trae esta capa, para los ".wh..wh..opq": por directorio de
	// verdad (el nodo) y no por la ruta del tar, que puede pasar por un
	// enlace (bin/x cae en usr/bin con bin -> usr/bin).
	type hijo struct {
		dir  *Node
		name string
	}
	added := map[hijo]bool{}
	tr := tar.NewReader(r)
	for idx := 0; ; idx++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar entry %d: %w", idx, err)
		}
		if o.Entries != nil {
			*o.Entries++
			if o.MaxEntries > 0 && *o.Entries > o.MaxEntries {
				return fmt.Errorf("more than %d tar entries", o.MaxEntries)
			}
		}
		name := path.Clean("/" + h.Name)
		if name == "/" {
			if h.Typeflag == tar.TypeDir {
				setMeta(base, h)
			}
			continue
		}
		for _, c := range strings.Split(name[1:], "/") {
			if c == ".." {
				return fmt.Errorf("tar entry %q escapes the root", h.Name)
			}
		}
		full := path.Join(baseDir, name)
		dir, file := path.Split(name)
		if o.Whiteouts && strings.HasPrefix(file, ".wh.") {
			// El directorio se resuelve siguiendo enlaces, como al meter
			// ficheros (MkdirAll): bin/.wh.x con bin -> usr/bin borra
			// usr/bin/x. Dentro de base: un enlace no lleva fuera.
			d, _ := base.Resolve(dir)
			if d == nil || !d.IsDir() {
				continue
			}
			if file == ".wh..wh..opq" {
				for _, c := range d.Children() {
					if !added[hijo{d, c}] {
						d.RemoveChild(c)
					}
				}
			} else {
				d.RemoveChild(strings.TrimPrefix(file, ".wh."))
			}
			continue
		}
		parent, err := base.MkdirAll(dir, 0o755, 0, 0, o.Time)
		if err != nil {
			return fmt.Errorf("%s: %w", full, err)
		}
		added[hijo{parent, file}] = true
		if o.OnEntry != nil {
			o.OnEntry(name, h)
		}
		var n *Node
		switch h.Typeflag {
		case tar.TypeDir:
			if old := parent.Child(file); old != nil && old.IsDir() {
				setMeta(old, h)
				continue
			} else if old != nil && old.IsLink() {
				// Como tar: un directorio que ya existe como enlace a un
				// directorio (/sbin -> usr/sbin) se deja como está.
				if r, _ := base.Resolve(name); r != nil && r.IsDir() {
					continue
				}
			}
			n = NewDir(0, 0, 0, time.Time{})
		case tar.TypeReg:
			n = &Node{Mode: ModeReg, Size: h.Size}
			if o.Keep != nil && h.Size <= maxKeep && o.Keep(full, h.Size) {
				b, err := io.ReadAll(tr)
				if err != nil {
					return fmt.Errorf("%s: %w", full, err)
				}
				n.Data = Bytes(b)
			} else if h.Size > 0 {
				n.Data = StreamKey{o.Stream, idx}
			}
		case tar.TypeSymlink:
			n = &Node{Mode: ModeLink, Target: h.Linkname}
		case tar.TypeLink:
			// El directorio del destino se resuelve siguiendo enlaces
			// (dentro de base, como MkdirAll): con /usr unificado,
			// bin/busybox vive en usr/bin. El último componente no se
			// sigue: un enlace duro a un enlace simbólico enlaza a este.
			var t *Node
			ld, lf := path.Split(path.Clean("/" + h.Linkname))
			if d, _ := base.Resolve(ld); d != nil && d.IsDir() && lf != "" {
				t = d.Child(lf)
			}
			if t == nil || t.IsDir() {
				return fmt.Errorf("%s: hard link to missing %q", full, h.Linkname)
			}
			parent.SetChild(file, t)
			continue
		case tar.TypeChar:
			n = &Node{Mode: ModeChar, Major: uint32(h.Devmajor), Minor: uint32(h.Devminor)}
		case tar.TypeBlock:
			n = &Node{Mode: ModeBlock, Major: uint32(h.Devmajor), Minor: uint32(h.Devminor)}
		case tar.TypeFifo:
			n = &Node{Mode: ModeFIFO}
		case tar.TypeXGlobalHeader:
			continue
		default:
			return fmt.Errorf("%s: unsupported tar entry type %q", full, h.Typeflag)
		}
		setMeta(n, h)
		parent.SetChild(file, n)
	}
}

func setMeta(n *Node, h *tar.Header) {
	n.Mode = n.Mode&ModeType | uint32(h.Mode)&ModePerm
	n.UID, n.GID = uint32(h.Uid), uint32(h.Gid)
	n.Mtime, n.Atime, n.Ctime = h.ModTime, h.AccessTime, h.ChangeTime
	n.Xattrs = nil
	for k, v := range h.PAXRecords {
		if name, ok := strings.CutPrefix(k, "SCHILY.xattr."); ok {
			if n.Xattrs == nil {
				n.Xattrs = map[string][]byte{}
			}
			n.Xattrs[name] = []byte(v)
		}
	}
}

// TarStream es el flujo que entrega los datos de un tar que se metió con
// AddTar. open se llama una vez, al escribir.
func TarStream(open func() (io.ReadCloser, error)) Stream {
	return func(emit func(int, io.Reader) error) error {
		rc, err := open()
		if err != nil {
			return err
		}
		defer rc.Close()
		// Con búfer: sin él, cada cabecera (512 bytes) y cada fichero
		// pequeño era una lectura al sistema. Aquí se leen los datos
		// enteros, así que no se pierde el Seek con el que AddTar salta.
		tr := tar.NewReader(bufio.NewReaderSize(rc, 1<<20))
		for idx := 0; ; idx++ {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("tar entry %d: %w", idx, err)
			}
			if h.Typeflag != tar.TypeReg {
				continue
			}
			if err := emit(idx, tr); err != nil {
				return err
			}
		}
	}
}
