package guest

// El servidor FUSE de una carpeta viva servido desde el HOST, por virtio-fs
// (kling-hv, vz/internal/hvvm/vfs.c): el mismo fuseFS que el agente usa con
// /dev/fuse, con las mismas validaciones y la misma sesión kling-share/1 hacia
// internal/share (os.Root), pero leyendo las peticiones de las colas de un
// dispositivo virtio-fs. Así el kernel del invitado habla FUSE con el host sin
// pasar por el agente ni por vsock. Ver docs/backend-hv.md.
//
// La tabla de nodos vive entonces en el proceso del VMM, que muere al
// congelar: HostState la guarda en el snapshot y NewHostFSFromState la
// recupera, porque el kernel restaurado sigue nombrando esos nodos y esos
// handles. Los ficheros abiertos se reabren por ruta en la sesión nueva, como
// en el agente (reopenLocked).

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"io"

	share "github.com/juan52878911/kindling/internal/shareproto"
)

// FuseDev es el dispositivo del que HostFS lee peticiones FUSE enteras (una
// por Read) y al que manda respuestas enteras (una por Write).
type FuseDev interface {
	Read(buf []byte) (int, error)
	Write(msg []byte) (int, error)
}

// HostFS es una carpeta viva servida por virtio-fs.
type HostFS struct {
	fs *fuseFS
}

// NewHostFS crea el servidor de la carpeta name (solo para los logs) sobre dev.
func NewHostFS(dev FuseDev, name string, readOnly bool) *HostFS {
	return &HostFS{fs: newHostFuseFS(dev, name, readOnly)}
}

func newHostFuseFS(dev FuseDev, name string, readOnly bool) *fuseFS {
	fs := newFuseFS(dev, name, readOnly)
	// Nada de inotify sintético: las operaciones que lo provocan tendrían que
	// hacerse dentro del invitado (fuse_notify.go), y virtio-fs no tiene cola
	// de avisos (FUSE_NOTIFY_*) en Linux.
	fs.touchHost = func(synthOp, string) {}
	return fs
}

// BufSize es lo que tiene que caber en el búfer de lectura: la escritura más
// grande que se negocia en INIT (max_write) con su cabecera.
const BufSize = fuseReadBuf

// Attach pone la sesión kling-share/1 que el daemon abrió sobre rwc
// (POST /kling/share/attach) y cierra la anterior. Devuelve lo negociado por
// operación. Sin WATCH: los avisos de cambios del host no tienen adónde ir
// (ver newHostFuseFS), y un cambio del host se ve al caducar la caché de 1 s.
// Con escritura diferida (fuse_wb.go): lo escrito y aún no mandado vive aquí,
// en el VMM, y no en la RAM del invitado como en el agente, así que State lo
// guarda con lo demás.
func (h *HostFS) Attach(rwc io.ReadWriteCloser) uint32 {
	sess := newShareSession(rwc)
	sess.hello()
	h.fs.attach(sess)
	return sess.maxIO
}

// Nodes es cuántos nodos tiene la tabla (para pruebas y diagnósticos).
func (h *HostFS) Nodes() int { return h.fs.nodes.size() }

// Estado guardado. Gob: es del mismo programa que lo lee, y la versión va
// dentro por si cambia.
const hostStateVersion = 1

type hostState struct {
	Version   int
	AutoInval bool
	Next      uint64
	Nodes     []hostNode
	NextFH    uint64
	Files     []hostFile
	Dirs      []hostDir
	Pending   []hostWB
}

// hostWB es lo pendiente de la escritura diferida de un nodo: lo que el
// invitado ya dio por escrito y no ha llegado al host. Los trozos van en el
// orden en que se mandarían; FH 0 es un handle que ya no está en la tabla
// (se usa Node y Flags).
type hostWB struct {
	Node   uint64
	Err    uint32
	Chunks []hostChunk
}

type hostChunk struct {
	FH, Node uint64
	Flags    uint32
	Off      uint64
	Data     []byte
	Cur      bool // el que se acumulaba (st.cur): sale cuando no vuele nada
}

type hostNode struct {
	ID, Parent uint64 // Parent 0: huérfano (se borró o lo pisó un rename)
	Name       string
	Nlookup    uint64
	Typ        uint32
}

type hostFile struct {
	FH, Node uint64
	Flags    uint32
}

type hostDir struct {
	FH, Node uint64
	Names    []string
	Types    []uint32
}

// State es lo que hay que guardar para seguir tras restaurar: nodos, ficheros
// y directorios abiertos. Se llama con la VM en pausa y los dispositivos
// quietos (tras el snapshot del C): lo que siga en vuelo lo repite el C al
// restaurar, y si ya tocó la tabla, a lo sumo deja una referencia de más (un
// nodo que no se suelta).
func (h *HostFS) State() ([]byte, error) {
	fs := h.fs
	st := hostState{Version: hostStateVersion, AutoInval: fs.autoInval.Load()}
	t := fs.nodes
	t.mu.Lock()
	st.Next = t.next
	st.Nodes = make([]hostNode, 0, len(t.byID))
	for _, n := range t.byID {
		if n == t.root {
			continue
		}
		var parent uint64
		if p := n.parent; p != nil && p.children[n.name] == n {
			parent = p.id
		}
		st.Nodes = append(st.Nodes, hostNode{ID: n.id, Parent: parent, Name: n.name, Nlookup: n.nlookup, Typ: n.typ.Load()})
	}
	t.mu.Unlock()
	fs.hmu.Lock()
	st.NextFH = fs.nextFH
	fhDe := map[*fileHandle]uint64{}
	for fh, f := range fs.files {
		f.mu.Lock()
		st.Files = append(st.Files, hostFile{FH: fh, Node: f.node.id, Flags: f.flags})
		f.mu.Unlock()
		fhDe[f] = fh
	}
	for fh, d := range fs.dirs {
		hd := hostDir{FH: fh, Node: d.node.id}
		for _, e := range d.entries {
			hd.Names = append(hd.Names, e.name)
			hd.Types = append(hd.Types, e.typ)
		}
		st.Dirs = append(st.Dirs, hd)
	}
	fs.hmu.Unlock()
	// Lo pendiente de la escritura diferida. Lo que esté volando se manda otra
	// vez al restaurar: escribir lo mismo en el mismo sitio dos veces da igual.
	t.mu.Lock()
	nodos := make([]*node, 0, len(t.byID))
	for _, n := range t.byID {
		nodos = append(nodos, n)
	}
	t.mu.Unlock()
	for _, n := range nodos {
		w := n.wb.Load()
		if w == nil {
			continue
		}
		w.mu.Lock()
		p := hostWB{Node: n.id, Err: w.err}
		cs := append([]*wbChunk(nil), w.flying...)
		if w.cur != nil {
			cs = append(cs, w.cur)
		}
		for _, c := range cs {
			hc := hostChunk{FH: fhDe[c.h], Off: c.off, Data: append([]byte(nil), c.data...), Cur: c == w.cur}
			c.h.mu.Lock()
			hc.Node, hc.Flags = c.h.node.id, c.h.flags
			c.h.mu.Unlock()
			p.Chunks = append(p.Chunks, hc)
		}
		w.mu.Unlock()
		if p.Err != 0 || len(p.Chunks) > 0 {
			st.Pending = append(st.Pending, p)
		}
	}
	var b bytes.Buffer
	if err := gob.NewEncoder(&b).Encode(&st); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// NewHostFSFromState es NewHostFS con lo que guardó State. El estado viene del
// snapshot (lo escribió este programa), pero se valida igual: ids repetidos,
// padres que no existen, ciclos o nombres inválidos lo rechazan entero.
func NewHostFSFromState(dev FuseDev, name string, readOnly bool, state []byte) (*HostFS, error) {
	var st hostState
	if err := gob.NewDecoder(bytes.NewReader(state)).Decode(&st); err != nil {
		return nil, fmt.Errorf("virtio-fs state: %w", err)
	}
	if st.Version != hostStateVersion {
		return nil, fmt.Errorf("virtio-fs state version %d, this build reads %d", st.Version, hostStateVersion)
	}
	if len(st.Nodes) > maxNodes || len(st.Files) > maxOpen || len(st.Dirs) > maxOpen {
		return nil, errors.New("virtio-fs state: more nodes or handles than the limits")
	}
	fs := newHostFuseFS(dev, name, readOnly)
	fs.autoInval.Store(st.AutoInval)
	t := fs.nodes
	for _, r := range st.Nodes {
		if r.ID <= rootID || r.ID >= st.Next || t.byID[r.ID] != nil {
			return nil, fmt.Errorf("virtio-fs state: bad node id %d", r.ID)
		}
		if r.Parent != 0 && share.ValidName(r.Name) != nil {
			return nil, fmt.Errorf("virtio-fs state: bad name %q", r.Name)
		}
		n := &node{id: r.ID, name: r.Name, nlookup: r.Nlookup}
		n.typ.Store(r.Typ)
		t.byID[r.ID] = n
	}
	for _, r := range st.Nodes {
		if r.Parent == 0 {
			continue
		}
		n, p := t.byID[r.ID], t.byID[r.Parent]
		if p == nil {
			return nil, fmt.Errorf("virtio-fs state: node %d has no parent %d", r.ID, r.Parent)
		}
		if p.children == nil {
			p.children = map[string]*node{}
		}
		if p.children[r.Name] != nil {
			return nil, fmt.Errorf("virtio-fs state: %q twice under node %d", r.Name, r.Parent)
		}
		p.children[r.Name] = n
		n.parent = p
	}
	// Sin ciclos: subiendo desde cada nodo se llega a la raíz o a un huérfano.
	for _, n := range t.byID {
		steps := 0
		for c := n; c != nil && c != t.root; c = c.parent {
			if steps++; steps > len(t.byID) {
				return nil, errors.New("virtio-fs state: the node table has a cycle")
			}
		}
	}
	t.next = st.Next
	fs.nextFH = st.NextFH
	for _, f := range st.Files {
		n := t.byID[f.Node]
		if n == nil || f.FH == 0 || f.FH > st.NextFH || fs.files[f.FH] != nil {
			return nil, fmt.Errorf("virtio-fs state: bad file handle %d", f.FH)
		}
		// Sin sesión: el primer uso lo reabre por ruta (handleCallRaw).
		fs.files[f.FH] = &fileHandle{node: n, flags: f.Flags}
	}
	for _, d := range st.Dirs {
		n := t.byID[d.Node]
		if n == nil || d.FH == 0 || d.FH > st.NextFH || fs.dirs[d.FH] != nil || fs.files[d.FH] != nil ||
			len(d.Names) != len(d.Types) || len(d.Names) > maxDirEntries {
			return nil, fmt.Errorf("virtio-fs state: bad directory handle %d", d.FH)
		}
		dh := &dirHandle{node: n, entries: make([]dirEntry, len(d.Names))}
		for i := range d.Names {
			dh.entries[i] = dirEntry{name: d.Names[i], typ: d.Types[i]}
		}
		fs.dirs[d.FH] = dh
	}
	// Lo pendiente vuelve como en vuelo: sale en cuanto haya sesión, y lo que
	// el kernel pida de esos nodos espera antes a que llegue (drain).
	for _, p := range st.Pending {
		n := t.byID[p.Node]
		if n == nil {
			return nil, fmt.Errorf("virtio-fs state: pending writes for a missing node %d", p.Node)
		}
		w := wbOf(n)
		w.err = p.Err
		for _, c := range p.Chunks {
			h := fs.files[c.FH]
			if c.FH == 0 || h == nil {
				hn := t.byID[c.Node]
				if hn == nil {
					return nil, fmt.Errorf("virtio-fs state: pending write for a missing node %d", c.Node)
				}
				h = &fileHandle{node: hn, flags: c.Flags}
			}
			ch := &wbChunk{h: h, off: c.Off, data: c.Data}
			w.bytes += len(c.Data)
			if c.Cur {
				// Puede solaparse con uno de los que vuelan: no sale con ellos.
				w.cur = ch
				continue
			}
			w.flying = append(w.flying, ch)
		}
	}
	return &HostFS{fs: fs}, nil
}

// Serve atiende el dispositivo hasta que su Read falla (se cerró). Lo
// pendiente que trajo el estado sale antes, en su goroutine.
func (h *HostFS) Serve() {
	h.fs.nodes.mu.Lock()
	var ws []*wbState
	for _, n := range h.fs.nodes.byID {
		if w := n.wb.Load(); w != nil {
			ws = append(ws, w)
		}
	}
	h.fs.nodes.mu.Unlock()
	for _, w := range ws {
		w.mu.Lock()
		vuelan := append([]*wbChunk(nil), w.flying...)
		if len(vuelan) == 0 && w.cur != nil {
			h.fs.wbSendLocked(w)
		}
		w.mu.Unlock()
		for _, c := range vuelan {
			go h.fs.wbFly(w, c)
		}
	}
	h.fs.serve()
}
