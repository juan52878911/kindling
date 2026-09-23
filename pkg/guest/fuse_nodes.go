package guest

// La tabla de nodos de una carpeta compartida en vivo.
//
// El kernel habla de nodos por número (nodeid); el daemon, de rutas. Esta tabla
// traduce: cada nodo sabe su padre y su nombre, y su ruta se reconstruye
// subiendo. Así un rename es mover una entrada, no reescribir las rutas de todo
// un subárbol, y el daemon no guarda nada por nodo que el invitado pueda hacer
// crecer.
//
// Vive en el invitado a propósito: sobrevive a que se corte la conexión
// (congelar, reiniciar el daemon), que es lo que permite seguir con el MISMO
// montaje y los mismos ids al volver.

import (
	"sort"
	"strings"
	"sync"
)

// rootID es el nodeid de la raíz, fijo en FUSE.
const rootID = 1

// maxNodes es el tope de la tabla. Un kernel que no manda FORGET (o que tiene
// tanta memoria que nunca suelta nada) no puede hacerla crecer sin límite.
const maxNodes = 1 << 18

type node struct {
	id       uint64
	parent   *node // nil en la raíz y en los nodos borrados
	name     string
	nlookup  uint64
	children map[string]*node
	lastUse  uint64
}

type nodeTable struct {
	mu   sync.Mutex
	byID map[uint64]*node
	root *node
	next uint64
	tick uint64
	max  int
}

func newNodeTable() *nodeTable {
	r := &node{id: rootID, nlookup: 1}
	return &nodeTable{byID: map[uint64]*node{rootID: r}, root: r, next: rootID + 1, max: maxNodes}
}

// get devuelve el nodo id, o nil si no existe (nunca existió, se olvidó o se
// expulsó: para el kernel, ESTALE).
func (t *nodeTable) get(id uint64) *node {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.byID[id]
	if n != nil {
		t.tick++
		n.lastUse = t.tick
	}
	return n
}

// path reconstruye la ruta de n relativa a la carpeta. false si el nodo está
// huérfano (se borró o se pisó con un rename): ya no tiene ruta.
func (t *nodeTable) path(n *node) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pathLocked(n)
}

func (t *nodeTable) pathLocked(n *node) (string, bool) {
	var parts []string
	for c := n; c != t.root; c = c.parent {
		if c == nil {
			return "", false
		}
		parts = append(parts, c.name)
		if len(parts) > 2048 {
			// Un ciclo sería un fallo nuestro; mejor una ruta inválida que un
			// bucle infinito en PID 1.
			return "", false
		}
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, "/"), true
}

// parentID es el id del padre de n (el propio n en la raíz o si está huérfano).
func (t *nodeTable) parentID(n *node) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n.parent != nil {
		return n.parent.id
	}
	return n.id
}

// child devuelve el hijo name de parent si está en la tabla, sin contarlo como
// referencia del kernel.
func (t *nodeTable) child(parent *node, name string) *node {
	t.mu.Lock()
	defer t.mu.Unlock()
	return parent.children[name]
}

// lookup devuelve (o crea) el hijo name de parent y le suma una referencia del
// kernel: cada LOOKUP, CREATE o MKDIR que contesta con un nodo es una.
func (t *nodeTable) lookup(parent *node, name string) *node {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tick++
	n := parent.children[name]
	if n == nil {
		n = &node{id: t.next, parent: parent, name: name}
		t.next++
		if parent.children == nil {
			parent.children = map[string]*node{}
		}
		parent.children[name] = n
		t.byID[n.id] = n
	}
	n.nlookup++
	n.lastUse = t.tick
	if len(t.byID) > t.max {
		t.evictLocked()
	}
	return n
}

// forget resta referencias y suelta el nodo cuando el kernel ya no tiene
// ninguna y no le quedan hijos en la tabla.
func (t *nodeTable) forget(id, count uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.byID[id]
	if n == nil || n == t.root {
		return
	}
	if count >= n.nlookup {
		n.nlookup = 0
	} else {
		n.nlookup -= count
	}
	t.dropLocked(n)
}

// dropLocked retira n, y a sus padres que se queden vacíos y sin referencias.
func (t *nodeTable) dropLocked(n *node) {
	for n != nil && n != t.root && n.nlookup == 0 && len(n.children) == 0 {
		delete(t.byID, n.id)
		p := n.parent
		if p != nil && p.children[n.name] == n {
			delete(p.children, n.name)
		}
		n.parent = nil
		n = p
	}
}

// detach deja huérfano al hijo name de parent (se borró, o un rename lo pisó).
// Sigue en la tabla mientras el kernel lo nombre, pero ya sin ruta.
func (t *nodeTable) detach(parent *node, name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.detachLocked(parent, name)
}

func (t *nodeTable) detachLocked(parent *node, name string) {
	n := parent.children[name]
	if n == nil {
		return
	}
	delete(parent.children, name)
	n.parent = nil
	t.dropLocked(n)
}

// rename mueve el nodo de (op, on) a (np, nn), dejando huérfano lo que hubiera
// en el destino.
func (t *nodeTable) rename(op *node, on string, np *node, nn string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := op.children[on]
	if n == nil {
		// No estaba en la tabla: el kernel nunca lo buscó. Solo hay que dejar
		// huérfano el destino, si lo estaba.
		t.detachLocked(np, nn)
		return
	}
	if dst := np.children[nn]; dst != nil && dst != n {
		t.detachLocked(np, nn)
	}
	delete(op.children, on)
	n.parent, n.name = np, nn
	if np.children == nil {
		np.children = map[string]*node{}
	}
	np.children[nn] = n
}

// evictLocked expulsa hojas usadas hace más tiempo hasta dejar la tabla por
// debajo del tope con margen. Un nodo expulsado que el kernel vuelva a nombrar
// recibe ESTALE, y el kernel lo vuelve a buscar por nombre: se recupera solo.
//
// Se expulsa en bloque (1/16 del tope) para que el coste de ordenar se pague
// una vez cada muchas altas y no en cada una.
func (t *nodeTable) evictLocked() {
	var leaves []*node
	for _, n := range t.byID {
		if n != t.root && len(n.children) == 0 {
			leaves = append(leaves, n)
		}
	}
	sort.Slice(leaves, func(i, j int) bool { return leaves[i].lastUse < leaves[j].lastUse })
	want := len(t.byID) - t.max + t.max/16
	for _, n := range leaves {
		if want <= 0 {
			break
		}
		delete(t.byID, n.id)
		if p := n.parent; p != nil && p.children[n.name] == n {
			delete(p.children, n.name)
		}
		n.parent = nil
		want--
	}
}

func (t *nodeTable) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.byID)
}
