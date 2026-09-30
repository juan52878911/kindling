// Package ext4 escribe y lee sistemas de ficheros ext4 en Go, sin mkfs,
// debugfs ni montar nada.
//
// Existe para construir imágenes en un host que no puede montar un loopback
// ni hacer chroot (un Mac), y para no depender de que e2fsprogs esté
// instalado donde corre el constructor. El escritor hace un ext4 de solo lo
// que el núcleo y e2fsck necesitan: bloques de 4 KiB, extents, flex_bg, sin
// journal, sin sumas de comprobación de metadatos y con directorios lineales.
// Es un ext4 válido de cabo a rabo (`e2fsck -fn` limpio, montable en
// escritura, `resize2fs` lo crece), pensado para imágenes que se escriben una
// vez y luego se montan de solo lectura.
//
// El modelo es un árbol en memoria (Node) cuyos ficheros regulares llevan una
// fuente de datos (Source): bytes, un fichero del host, extents de otra imagen
// ext4, o un flujo (el tar de una capa OCI) que se recorre una sola vez al
// escribir. Así una capa de 2 GiB se escribe sin extraerla a disco antes: en
// un Mac eso ni siquiera sería posible, porque APFS no guarda dueños de otros
// usuarios, ni xattrs security.*, y no distingue mayúsculas.
package ext4

import (
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

// Tipos de fichero en st_mode.
const (
	ModeType   = 0o170000
	ModeSocket = 0o140000
	ModeLink   = 0o120000
	ModeReg    = 0o100000
	ModeBlock  = 0o060000
	ModeDir    = 0o040000
	ModeChar   = 0o020000
	ModeFIFO   = 0o010000
	ModePerm   = 0o7777 // permisos + setuid, setgid y sticky
)

// Node es un fichero del árbol. Un mismo *Node colgado de varios directorios
// es un enlace duro (solo para lo que no es directorio).
type Node struct {
	// Mode es el st_mode completo: tipo, permisos y setuid/setgid/sticky.
	Mode     uint32
	UID, GID uint32
	// Mtime manda: Atime y Ctime valen Mtime si son cero.
	Mtime, Atime, Ctime time.Time
	// Xattrs con su nombre completo ("security.capability", "user.x").
	Xattrs map[string][]byte
	// Target es el destino de un enlace simbólico.
	Target string
	// Major y Minor de un dispositivo de caracteres o de bloques.
	Major, Minor uint32
	// Size y Data de un fichero regular.
	Size int64
	Data Source

	children map[string]*Node

	// del escritor
	ino   uint32
	nlink int
}

// Source es de dónde salen los bytes de un fichero regular: Bytes, HostFile,
// Extents o StreamKey.
type Source interface{ isSource() }

// Bytes son los datos en memoria.
type Bytes []byte

// HostFile es un fichero del host que se lee al escribir la imagen.
type HostFile struct{ Path string }

// StreamKey dice que los datos llegan por el flujo Stream (índice en la lista
// que recibe Write) con la clave Key.
type StreamKey struct {
	Stream int
	Key    int
}

// Extents son los datos de un fichero de otra imagen ext4 (Read).
type Extents struct {
	r    io.ReaderAt
	runs []run
}

type run struct {
	logical, phys, n uint64 // bloques; phys 0 = hueco
}

func (Bytes) isSource()     {}
func (HostFile) isSource()  {}
func (StreamKey) isSource() {}
func (*Extents) isSource()  {}

// NewDir crea un directorio vacío.
func NewDir(perm uint32, uid, gid uint32, t time.Time) *Node {
	return &Node{Mode: ModeDir | perm&ModePerm, UID: uid, GID: gid, Mtime: t, children: map[string]*Node{}}
}

// IsDir dice si el nodo es un directorio.
func (n *Node) IsDir() bool { return n.Mode&ModeType == ModeDir }

// IsReg dice si el nodo es un fichero regular.
func (n *Node) IsReg() bool { return n.Mode&ModeType == ModeReg }

// IsLink dice si el nodo es un enlace simbólico.
func (n *Node) IsLink() bool { return n.Mode&ModeType == ModeLink }

// Children devuelve los nombres de un directorio, ordenados.
func (n *Node) Children() []string {
	names := make([]string, 0, len(n.children))
	for k := range n.children {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Child devuelve la entrada name de un directorio, o nil.
func (n *Node) Child(name string) *Node {
	if n.children == nil {
		return nil
	}
	return n.children[name]
}

// SetChild pone (o sustituye) la entrada name de un directorio.
func (n *Node) SetChild(name string, c *Node) {
	if n.children == nil {
		n.children = map[string]*Node{}
	}
	n.children[name] = c
}

// RemoveChild quita la entrada name de un directorio.
func (n *Node) RemoveChild(name string) { delete(n.children, name) }

// ClearChildren vacía un directorio (el ".wh..wh..opq" de una capa OCI).
func (n *Node) ClearChildren() { n.children = map[string]*Node{} }

// splitPath parte una ruta dentro del árbol en componentes, sin "." ni "..".
func splitPath(p string) ([]string, error) {
	p = path.Clean("/" + p)
	if p == "/" {
		return nil, nil
	}
	parts := strings.Split(p[1:], "/")
	for _, c := range parts {
		if c == "" || c == "." || c == ".." || len(c) > 255 {
			return nil, fmt.Errorf("invalid path component %q", c)
		}
	}
	return parts, nil
}

// Lookup busca una ruta sin seguir enlaces simbólicos.
func (n *Node) Lookup(p string) *Node {
	parts, err := splitPath(p)
	if err != nil {
		return nil
	}
	cur := n
	for _, c := range parts {
		if !cur.IsDir() {
			return nil
		}
		cur = cur.children[c]
		if cur == nil {
			return nil
		}
	}
	return cur
}

// Resolve busca una ruta siguiendo enlaces simbólicos DENTRO del árbol (un
// destino absoluto cuelga de n, no del host). Hasta 40 saltos.
func (n *Node) Resolve(p string) (*Node, string) {
	parts, err := splitPath(p)
	if err != nil {
		return nil, ""
	}
	return n.resolve(parts, 0)
}

func (n *Node) resolve(parts []string, hops int) (*Node, string) {
	cur, curPath := n, "/"
	for i := 0; i < len(parts); i++ {
		if !cur.IsDir() {
			return nil, ""
		}
		c := cur.children[parts[i]]
		if c == nil {
			return nil, ""
		}
		if c.IsLink() && hops < 40 {
			t := c.Target
			if !strings.HasPrefix(t, "/") {
				t = path.Join(curPath, t)
			}
			rest, err := splitPath(t)
			if err != nil {
				return nil, ""
			}
			return n.resolve(append(rest, parts[i+1:]...), hops+1)
		}
		cur, curPath = c, path.Join(curPath, parts[i])
	}
	return cur, curPath
}

// MkdirAll crea los directorios que falten de p (con el modo, dueño y hora
// dados) y devuelve el último. Sigue los enlaces simbólicos a directorios
// que encuentre por el camino (dentro del árbol, con n como raíz), como hace
// tar al extraer: en una base con /usr unificado, /sbin/x cae en /usr/sbin/x.
// Falla si algo del camino no es directorio.
func (n *Node) MkdirAll(p string, perm, uid, gid uint32, t time.Time) (*Node, error) {
	parts, err := splitPath(p)
	if err != nil {
		return nil, err
	}
	cur := n
	for i, c := range parts {
		nx := cur.children[c]
		if nx != nil && nx.IsLink() {
			if r, _ := n.Resolve("/" + strings.Join(parts[:i+1], "/")); r != nil && r.IsDir() {
				nx = r
			}
		}
		if nx == nil {
			nx = NewDir(perm, uid, gid, t)
			cur.children[c] = nx
		} else if !nx.IsDir() {
			return nil, fmt.Errorf("/%s is not a directory", strings.Join(parts[:i+1], "/"))
		}
		cur = nx
	}
	return cur, nil
}

// Put cuelga c en la ruta p, creando los directorios padre que falten
// (0755, root). Sustituye lo que hubiera.
func (n *Node) Put(p string, c *Node, t time.Time) error {
	dir, base := path.Split(path.Clean("/" + p))
	if base == "" {
		return fmt.Errorf("invalid path %q", p)
	}
	d, err := n.MkdirAll(dir, 0o755, 0, 0, t)
	if err != nil {
		return err
	}
	d.children[base] = c
	return nil
}

// Remove quita la ruta p (y todo lo de debajo, si es un directorio). No es
// error que no exista.
func (n *Node) Remove(p string) {
	dir, base := path.Split(path.Clean("/" + p))
	if d := n.Lookup(dir); d != nil && d.IsDir() {
		delete(d.children, base)
	}
}

// Walk recorre el árbol en orden (padres antes que hijos, hermanos por
// nombre). fn recibe la ruta absoluta; un enlace duro sale una vez por ruta.
func (n *Node) Walk(fn func(p string, n *Node) error) error {
	return n.walk("/", fn)
}

func (n *Node) walk(p string, fn func(string, *Node) error) error {
	if err := fn(p, n); err != nil {
		return err
	}
	if !n.IsDir() {
		return nil
	}
	for _, name := range n.Children() {
		if err := n.children[name].walk(path.Join(p, name), fn); err != nil {
			return err
		}
	}
	return nil
}

// Open devuelve un lector de los datos de un fichero regular, para las
// fuentes que se pueden leer en cualquier momento (no StreamKey).
func (n *Node) Open() (io.ReadCloser, error) {
	switch d := n.Data.(type) {
	case nil:
		if n.Size == 0 {
			return io.NopCloser(strings.NewReader("")), nil
		}
		return nil, fmt.Errorf("no data for a %d-byte file", n.Size)
	case Bytes:
		return io.NopCloser(strings.NewReader(string(d))), nil
	case HostFile:
		return openHost(d.Path, n.Size)
	case *Extents:
		return io.NopCloser(io.NewSectionReader(&extentReader{d, n.Size}, 0, n.Size)), nil
	case StreamKey:
		return nil, fmt.Errorf("streamed data can only be read while writing")
	}
	return nil, fmt.Errorf("unknown source %T", n.Data)
}

// ReadAll lee los datos entero (para ficheros pequeños que hay que editar).
func (n *Node) ReadAll() ([]byte, error) {
	r, err := n.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// extentReader lee un fichero de otra imagen por sus extents.
type extentReader struct {
	e    *Extents
	size int64
}

func (x *extentReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= x.size {
		return 0, io.EOF
	}
	if int64(len(p)) > x.size-off {
		p = p[:x.size-off]
	}
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		blk := uint64(pos / blockSize)
		in := pos % blockSize
		want := int(blockSize - in)
		if want > len(p)-n {
			want = len(p) - n
		}
		phys := uint64(0)
		for _, r := range x.e.runs {
			if blk >= r.logical && blk < r.logical+r.n {
				if r.phys != 0 {
					phys = r.phys + (blk - r.logical)
				}
				break
			}
		}
		if phys == 0 {
			clear(p[n : n+want])
		} else if _, err := x.e.r.ReadAt(p[n:n+want], int64(phys)*blockSize+in); err != nil {
			return n, err
		}
		n += want
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
