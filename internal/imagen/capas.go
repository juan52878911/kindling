package imagen

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/pkg/api"
)

// Reutilizar una base ya importada. Una imagen OCI se identifica por su
// cadena de diff_ids (el sha256 de cada capa SIN comprimir: dos
// reconstrucciones de la misma capa, con otro gzip, dan otro digest de blob
// pero el mismo diff_id). Si el prefijo de la cadena de una imagen es la
// cadena entera de otra ya importada, la nueva se guarda como una capa de
// kindling (<nombre>.layer.ext4) sobre la antigua, con solo la diferencia, en
// vez de otro ext4 completo.

// BaseOCI es una imagen OCI ya importada, entera en su .ext4, que puede hacer
// de base.
type BaseOCI struct {
	Name    string   `json:"name"`
	Arch    string   `json:"arch"`
	DiffIDs []string `json:"diff_ids"`
	// Init es "sh" o "go": el /sbin/overlay-init que lleva. Una capa nueva no
	// lo puede cambiar (se lee de la base antes de montarla).
	Init string `json:"init"`
	// Agent es el sha256 del kling-guest que lleva dentro.
	Agent string `json:"agent"`
}

// BasesFile es el fichero del directorio de trabajo de una construcción donde
// el daemon deja el índice de bases (el constructor sin privilegios no puede
// leer images/).
const BasesFile = "oci-bases.json"

// LeerBases lee el índice que dejó el daemon; ok es false si no hay.
func LeerBases(dir string) (bases []BaseOCI, ok bool) {
	b, err := os.ReadFile(filepath.Join(dir, BasesFile))
	if err != nil || json.Unmarshal(b, &bases) != nil {
		return nil, false
	}
	return bases, true
}

// EscribirBases deja el índice de images en dir, para LeerBases.
func EscribirBases(images, dir string) (string, error) {
	b, err := json.Marshal(IndexBases(images))
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, BasesFile)
	return p, os.WriteFile(p, b, 0o600)
}

// BuiltOCI es lo que el constructor oci apunta en Built de la receta para
// que otra importación lo encuentre.
type BuiltOCI struct {
	Arch    string   `json:"arch"`
	DiffIDs []string `json:"diff_ids"`
	Init    string   `json:"init"`
	Agent   string   `json:"agent_sha256"`
}

// IndexBases lee las recetas de images y devuelve las bases posibles: las
// imágenes del constructor oci, monolíticas (ni son
// una capa ni tienen base) y cuyo .ext4 sigue ahí. Una base borrada no
// aparece, aunque su receta se haya quedado.
func IndexBases(images string) []BaseOCI {
	recs, _ := filepath.Glob(filepath.Join(images, "*.recipe.json"))
	var out []BaseOCI
	for _, p := range recs {
		name := strings.TrimSuffix(filepath.Base(p), ".recipe.json")
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var rec api.ImageRecipe
		if json.Unmarshal(b, &rec) != nil || rec.Builder != "oci" || rec.Base != "" {
			continue
		}
		var built BuiltOCI
		if json.Unmarshal(rec.Built, &built) != nil || len(built.DiffIDs) == 0 || built.Init == "" {
			continue
		}
		if fi, err := os.Stat(filepath.Join(images, name+".ext4")); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if _, err := os.Stat(filepath.Join(images, name+".layer.ext4")); err == nil {
			continue
		}
		out = append(out, BaseOCI{Name: name, Arch: built.Arch, DiffIDs: built.DiffIDs, Init: built.Init, Agent: built.Agent})
	}
	return out
}

// BestBase es la base cuya cadena es un prefijo (propio: la imagen tiene que
// aportar al menos una capa más) de chain, la más larga; y con el mismo
// agente. Sin ninguna, nil. A igual largo gana el nombre menor, para que sea
// determinista.
func BestBase(bases []BaseOCI, chain []string, agent, arch string) *BaseOCI {
	var best *BaseOCI
	for i := range bases {
		b := &bases[i]
		if len(b.DiffIDs) >= len(chain) || b.Agent != agent || b.Arch != arch || !prefijo(b.DiffIDs, chain) {
			continue
		}
		if best == nil || len(b.DiffIDs) > len(best.DiffIDs) || (len(b.DiffIDs) == len(best.DiffIDs) && b.Name < best.Name) {
			best = b
		}
	}
	return best
}

func prefijo(p, chain []string) bool {
	if len(p) > len(chain) {
		return false
	}
	for i := range p {
		if p[i] != chain[i] {
			return false
		}
	}
	return true
}

// ErrPrefijoCompartido: la parte nueva enlaza (hard link) a un fichero de la
// base; en la capa no hay de dónde sacar sus datos. El que llama construye la
// imagen entera.
var ErrPrefijoCompartido = errors.New("a new layer hard-links a file of the base")

// Foto es el estado de un árbol en un momento, para sacar luego qué cambió.
type Foto struct{ root *foto }

type foto struct {
	n    *ext4.Node
	meta ext4.Node // copia de los campos de un directorio (sin hijos)
	xatt map[string][]byte
	kids map[string]*foto
}

// Fotografiar apunta el árbol tal como está.
func Fotografiar(root *ext4.Node) *Foto { return &Foto{hacerFoto(root)} }

func hacerFoto(n *ext4.Node) *foto {
	f := &foto{n: n, xatt: copiaXattrs(n.Xattrs)}
	f.meta = ext4.Node{Mode: n.Mode, UID: n.UID, GID: n.GID, Mtime: n.Mtime, Atime: n.Atime, Ctime: n.Ctime}
	if n.IsDir() {
		f.kids = map[string]*foto{}
		for _, c := range n.Children() {
			f.kids[c] = hacerFoto(n.Child(c))
		}
	}
	return f
}

func copiaXattrs(m map[string][]byte) map[string][]byte {
	if m == nil {
		return nil
	}
	o := make(map[string][]byte, len(m))
	for k, v := range m {
		o[k] = append([]byte(nil), v...)
	}
	return o
}

// igualDir dice si el directorio n sigue con los mismos atributos que en la
// foto (los ficheros no se tocan en sitio: un nodo distinto es un fichero
// distinto).
func (f *foto) igualDir(n *ext4.Node) bool {
	m := f.meta
	return n == f.n && m.Mode == n.Mode && m.UID == n.UID && m.GID == n.GID && m.Mtime.Equal(n.Mtime) &&
		m.Atime.Equal(n.Atime) && m.Ctime.Equal(n.Ctime) && reflect.DeepEqual(f.xatt, n.Xattrs)
}

// Upper es lo que hay que poner encima de la foto para llegar a root: lo
// nuevo o cambiado, y un dispositivo de caracteres 0/0 (la marca de borrado de
// overlayfs) por cada entrada que ya no está. minStream es el primer flujo
// que es de la parte nueva: si algún fichero de la diferencia saca sus datos
// de uno anterior, ErrPrefijoCompartido.
func (f *Foto) Upper(root *ext4.Node, minStream int) (*ext4.Node, error) {
	up, err := difDir(f.root, root, minStream, true)
	if err != nil {
		return nil, err
	}
	return up, nil
}

// copiaDir es un directorio nuevo con los atributos de n y sin hijos.
func copiaDir(n *ext4.Node) *ext4.Node {
	d := ext4.NewDir(n.Mode&ext4.ModePerm, n.UID, n.GID, n.Mtime)
	d.Atime, d.Ctime, d.Xattrs = n.Atime, n.Ctime, copiaXattrs(n.Xattrs)
	return d
}

// Whiteout es la marca de borrado de overlayfs.
func Whiteout() *ext4.Node { return &ext4.Node{Mode: ext4.ModeChar} }

func difDir(f *foto, n *ext4.Node, minStream int, raiz bool) (*ext4.Node, error) {
	d := copiaDir(n)
	cambia := raiz || !f.igualDir(n)
	for _, name := range n.Children() {
		c := n.Child(name)
		fk := f.kids[name]
		switch {
		case fk == nil:
			if err := nuevo(c, minStream); err != nil {
				return nil, err
			}
			d.SetChild(name, c)
			cambia = true
		case c.IsDir() && fk.n.IsDir():
			sub, err := difDir(fk, c, minStream, false)
			if err != nil {
				return nil, err
			}
			if sub != nil {
				d.SetChild(name, sub)
				cambia = true
			}
		case c == fk.n && !c.IsDir():
			// igual
		default:
			if err := nuevo(c, minStream); err != nil {
				return nil, err
			}
			d.SetChild(name, c)
			cambia = true
		}
	}
	for name := range f.kids {
		if n.Child(name) == nil {
			d.SetChild(name, Whiteout())
			cambia = true
		}
	}
	if !cambia {
		return nil, nil
	}
	return d, nil
}

// nuevo comprueba que lo que entra entero en la capa saca sus datos de la
// parte nueva.
func nuevo(n *ext4.Node, minStream int) error {
	if n.IsDir() {
		for _, c := range n.Children() {
			if err := nuevo(n.Child(c), minStream); err != nil {
				return err
			}
		}
		return nil
	}
	if k, ok := n.Data.(ext4.StreamKey); ok && k.Stream < minStream {
		return ErrPrefijoCompartido
	}
	return nil
}
