package guest

// Avisos de cambios del host (share.OpWatch): de un aviso del daemon a la
// caché del kernel del invitado y a sus vigilantes de inotify.
//
// Dos cosas por aviso:
//
//  1. Invalidar: FUSE_NOTIFY_INVAL_INODE (atributos y páginas de un fichero),
//     FUSE_NOTIFY_INVAL_ENTRY (un nombre) y FUSE_NOTIFY_DELETE. El invitado ve
//     el cambio en milisegundos en vez de al caducar la caché (1 s).
//  2. Que inotify dispare. Un aviso de FUSE no genera eventos de inotify: solo
//     los genera una operación que pasa por el VFS del invitado. Así que el
//     agente HACE esa operación sobre el montaje (truncar a su propio tamaño
//     -> IN_MODIFY; crear con O_EXCL o mkdir -> IN_CREATE), desde un hilo
//     suyo, y contesta él mismo las peticiones FUSE que provoca, sin llegar al
//     host: el host no cambia y no hay eco. Se reconocen por el pid de la
//     cabecera FUSE, que es el id del hilo que hace la llamada.
//
// Se descartó un vigilante por sondeo en el invitado (cuesta CPU y latencia
// aun sin cambios, y no hace disparar a inotify) y emular inotify fuera del
// kernel (habría que cambiar cada herramienta). Los borrados van con
// FUSE_NOTIFY_DELETE, que el kernel convierte él solo en IN_DELETE.
//
// En una carpeta ro el VFS no deja truncar ni crear: se invalida, pero
// inotify no dispara.

import (
	"log"
	"strings"
	"sync/atomic"

	share "github.com/juan52878911/kindling/internal/shareproto"
)

// Códigos de aviso de FUSE (fuse_notify_code).
const (
	notifyInvalInode = 2
	notifyInvalEntry = 3
	notifyDelete     = 6
)

// synthOp es la operación que el notificador está haciendo sobre el montaje:
// las peticiones que provoque, de su hilo y sobre estos nodos, las contesta
// synthReply.
type synthOp struct {
	ev     byte   // share.EventModify o EventCreate; 0 = ninguna
	node   uint64 // modify: el fichero
	parent uint64 // create: el directorio
	name   string // create: el nombre
	dir    bool   // create: es un directorio
	path   string // ruta del protocolo
}

var droppedEvents atomic.Uint64

// enqueue pone un aviso en la cola del notificador (que arranca con el
// primero). Si la cola está llena se pierde, y se apunta (perdidos).
func (fs *fuseFS) enqueue(ev pushEvent) {
	fs.evOnce.Do(func() { go fs.notifyLoop() })
	select {
	case fs.evq <- ev:
	default:
		fs.perdidos.Store(true)
		if droppedEvents.Add(1) == 1 {
			log.Printf("share %s: too many host changes at once; the guest's caches are dropped and kept short until it catches up", fs.mount)
		}
	}
}

// Avisos perdidos. Con la caché larga (30 s) no se sabe qué entrada quedó
// vieja, así que se tira todo lo que el kernel tiene (invalidarTodo) y,
// mientras la cola no se vacía, ttls da 1 s a lo nuevo. Se invalida dos
// veces: en cuanto se nota la pérdida, para que lo viejo no dure lo que dure
// la ráfaga, y al vaciarse la cola, por lo que se diera largo entre medias
// (una respuesta que salió justo antes de marcar la pérdida). Después ya no
// hay nada en caché que dependa de un aviso perdido y se vuelve a la caché
// larga. Una cola llena implica avisos detrás, así que el notificador siempre
// vuelve a pasar por aquí tras una pérdida.
func (fs *fuseFS) notifyLoop() {
	lockThread()
	fs.synthTID.Store(gettid())
	enDesborde := false
	for ev := range fs.evq {
		if !enDesborde && fs.perdidos.Load() {
			enDesborde = true
			fs.invalidarTodo()
		}
		fs.apply(ev)
		if len(fs.evq) == 0 && fs.perdidos.Swap(false) {
			enDesborde = false
			fs.invalidarTodo()
		}
	}
}

// invalidarTodo tira de la caché del kernel todo lo que el agente le ha dado:
// la entrada de cada nodo en su padre (en un directorio, d_invalidate poda
// también lo que cuelga de él, negativas incluidas) y sus atributos y
// páginas. Las negativas de la raíz no se alcanzan, por eso van con 1 s.
func (fs *fuseFS) invalidarTodo() {
	for _, e := range fs.nodes.entradas() {
		if e.parent != 0 {
			fs.notifyEntry(e.parent, e.name)
		}
		fs.notifyInode(e.id)
	}
}

// walk devuelve el nodo de una ruta del protocolo si el kernel lo conoce.
func (fs *fuseFS) walk(p string) *node {
	n := fs.nodes.root
	if p == "" {
		return n
	}
	for _, c := range strings.Split(p, "/") {
		if n = fs.nodes.child(n, c); n == nil {
			return nil
		}
	}
	return n
}

func splitPath(p string) (string, string) {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}

// apply lleva un aviso al kernel. Lo que el kernel no conoce no necesita
// aviso: no puede tenerlo en caché ni tener un vigilante encima.
func (fs *fuseFS) apply(ev pushEvent) {
	if ev.path == "" {
		if ev.ev == eventForget {
			fs.invalidarTodo()
		}
		return
	}
	dir, name := splitPath(ev.path)
	parent := fs.walk(dir)
	if parent == nil {
		return
	}
	child := fs.nodes.child(parent, name)
	full := fs.mount + "/" + ev.path
	switch ev.ev {
	case eventForget:
		// El daemon ya no vigila la ruta: fuera de la caché, sin operación
		// sintética (no cambió nada). El próximo LOOKUP la vuelve a vigilar.
		fs.notifyEntry(parent.id, name)
		if child != nil {
			fs.notifyInode(child.id)
		}
	case share.EventModify:
		if child == nil {
			return
		}
		if child.typ.Load() == share.SIFDIR {
			// De un directorio el daemon solo dice «cambió» cuando no tiene
			// su foto: no sabe qué nombre apareció. Las negativas de dentro
			// caen con la entrada del directorio.
			fs.notifyEntry(parent.id, name)
		}
		fs.notifyInode(child.id)
		// El toque solo sobre ficheros regulares: el de un enlace seguiría el
		// enlace y la petición llegaría con otro nodo.
		if !fs.readOnly && child.typ.Load() == share.SIFREG {
			fs.synth(synthOp{ev: share.EventModify, node: child.id, path: ev.path}, full)
		}
	case share.EventCreate:
		fs.notifyEntry(parent.id, name)
		fs.notifyInode(parent.id)
		if fs.readOnly {
			return
		}
		errno, d := fs.call(share.OpStat, func(e *share.Enc) { e.Str(ev.path) })
		if errno != 0 {
			return
		}
		at := share.GetAttr(d)
		if d.Err() != nil {
			return
		}
		switch at.Mode & share.SIFMT {
		case share.SIFDIR, share.SIFREG:
			fs.synth(synthOp{ev: share.EventCreate, parent: parent.id, name: name,
				dir: at.Mode&share.SIFMT == share.SIFDIR, path: ev.path}, full)
		}
	case share.EventDelete:
		if child == nil {
			fs.notifyEntry(parent.id, name)
			return
		}
		typ := child.typ.Load()
		if !fs.readOnly && (typ == share.SIFREG || typ == share.SIFDIR) {
			fs.synth(synthOp{ev: share.EventDelete, node: child.id, parent: parent.id, name: name,
				dir: typ == share.SIFDIR, path: ev.path}, full)
		}
		// Si el borrado sintético no llegó (ro, o el kernel no quiso), que al
		// menos no quede en caché. Si llegó, el kernel ya no tiene la entrada
		// y esto no hace nada.
		if c := fs.nodes.child(parent, name); c != nil {
			fs.notifyDel(parent.id, c.id, name)
			// Y fuera de la tabla: si el host vuelve a crear el nombre, es otro
			// nodo, no el borrado.
			fs.nodes.detach(parent, name)
		}
		fs.notifyInode(parent.id)
	}
}

// synth hace la operación op sobre el montaje con synthReply activo.
func (fs *fuseFS) synth(op synthOp, full string) {
	if fs.touchHost == nil {
		return
	}
	fs.synMu.Lock()
	fs.syn = op
	fs.synMu.Unlock()
	fs.touchHost(op, full)
	fs.synMu.Lock()
	fs.syn = synthOp{}
	fs.synMu.Unlock()
}

// synthReply contesta, sin ir al host, las peticiones que provoca synth.
//
// Toda petición mutadora del hilo del notificador se queda aquí, la reconozca
// synthOpReply o no: el toque existe solo para que inotify dispare, y si su
// ruta acabó en otro nodo (un enlace, un nodo sustituido entre apply y el
// toque) mandarla al host cambiaría datos de verdad.
func (fs *fuseFS) synthReply(req fuseReq, n *node) ([]byte, bool) {
	if out, ok := fs.synthOpReply(req, n); ok {
		return out, true
	}
	if mutadora(req) {
		return reply(req.unique, share.EPERM), true
	}
	return nil, false
}

// mutadora dice si una petición puede cambiar algo en el host. Un OPEN solo
// si abre para escribir o trunca.
func mutadora(req fuseReq) bool {
	switch req.opcode {
	case opSetattr, opSymlink, opMknod, opMkdir, opUnlink, opRmdir, opRename,
		opRename2, opLink, opWrite, opCreate:
		return true
	case opOpen:
		if len(req.arg) < 4 {
			return true
		}
		f := le.Uint32(req.arg[0:])
		return f&linuxOAccMode != linuxORdonly || f&linuxOTrunc != 0
	}
	return false
}

// synthOpReply contesta las peticiones que corresponden a la operación en
// curso.
func (fs *fuseFS) synthOpReply(req fuseReq, n *node) ([]byte, bool) {
	fs.synMu.Lock()
	op := fs.syn
	fs.synMu.Unlock()
	u, a := req.unique, req.arg
	switch {
	case op.ev == share.EventModify && req.opcode == opSetattr && n.id == op.node:
		// El tamaño que se pidió es el que tiene: basta con los atributos.
		return fs.statReply(u, n), true

	case op.ev == share.EventDelete:
		return fs.synthDelete(req, n, op)

	case op.ev == share.EventCreate && n.id == op.parent:
		var name string
		switch req.opcode {
		case opLookup:
			name, _, _ = cstring(a)
		case opCreate:
			if len(a) >= 16 {
				name, _, _ = cstring(a[16:])
			}
		case opMkdir:
			if len(a) >= 8 {
				name, _, _ = cstring(a[8:])
			}
		default:
			return nil, false
		}
		if name != op.name {
			return nil, false
		}
		switch req.opcode {
		case opLookup:
			// Que el kernel crea que no existe, para que la cree.
			return reply(u, share.ENOENT), true
		case opMkdir:
			errno, d := fs.call(share.OpStat, func(e *share.Enc) { e.Str(op.path) })
			if errno != 0 {
				return reply(u, errno), true
			}
			at := share.GetAttr(d)
			if d.Err() != nil {
				return reply(u, share.EIO), true
			}
			c := fs.nodes.lookup(n, name)
			c.typ.Store(at.Mode & share.SIFMT)
			return reply(u, 0, entryOut(c.id, toFattr(at, c.id))), true
		case opCreate:
			errno, d, s := fs.callS(share.OpOpen, func(e *share.Enc) { e.Str(op.path); e.U32(share.FlagRead) })
			if errno != 0 {
				return reply(u, errno), true
			}
			dh := d.U64()
			at := share.GetAttr(d)
			if d.Err() != nil {
				return reply(u, share.EIO), true
			}
			c := fs.nodes.lookup(n, name)
			c.typ.Store(at.Mode & share.SIFMT)
			fh, e := fs.addFile(&fileHandle{node: c, flags: share.FlagRead, dh: dh, sess: s})
			if e != 0 {
				s.call(share.OpRelease, func(e *share.Enc) { e.U64(dh) })
				fs.nodes.forget(c.id, 1)
				return reply(u, e), true
			}
			return reply(u, 0, entryOut(c.id, toFattr(at, c.id)), openOut(fh)), true
		}
	}
	return nil, false
}

// synthDelete contesta lo que provoca el unlink o rmdir sintético: el kernel
// tiene que encontrar la entrada (aunque en el host ya no esté) y el borrado
// se da por hecho sin ir al host.
func (fs *fuseFS) synthDelete(req fuseReq, n *node, op synthOp) ([]byte, bool) {
	u, a := req.unique, req.arg
	fake := func() fattr {
		perm := uint32(0o644)
		if op.dir {
			perm = 0o755
		}
		return fattr{ino: op.node, mode: typeOf(op.dir) | perm, nlink: 1, blksize: 4096}
	}
	switch req.opcode {
	case opLookup, opUnlink, opRmdir:
		name, _, _ := cstring(a)
		if n.id != op.parent || name != op.name {
			return nil, false
		}
		if req.opcode == opLookup {
			c := fs.nodes.lookup(n, name)
			if c.id != op.node {
				// Otro nodo con ese nombre: mejor no inventar nada.
				fs.nodes.forget(c.id, 1)
				return reply(u, share.ENOENT), true
			}
			return reply(u, 0, entryOut(c.id, fake())), true
		}
		fs.nodes.detach(n, name)
		return reply(u, 0), true
	case opGetattr:
		if n.id == op.node {
			return reply(u, 0, attrOut(fake())), true
		}
	}
	return nil, false
}

func typeOf(dir bool) uint32 {
	if dir {
		return share.SIFDIR
	}
	return share.SIFREG
}

// ── avisos al kernel ────────────────────────────────────────────────────────
//
// Se escriben en /dev/fuse sin pasar por fs.wmu: una invalidación puede
// esperar a una página que está leyendo otra petición, y esa respuesta
// necesita escribir. Cada write a /dev/fuse es un mensaje entero (no hace
// falta serializarlos); el error (ENOENT: el kernel no lo tenía) no importa.

func notifyMsg(code uint32, payload []byte) []byte {
	b := make([]byte, outHeaderSize+len(payload))
	le.PutUint32(b[0:], uint32(len(b)))
	le.PutUint32(b[4:], code)
	// unique 0: es un aviso, no una respuesta.
	copy(b[outHeaderSize:], payload)
	return b
}

func (fs *fuseFS) notifyInode(id uint64) {
	p := make([]byte, 24)
	le.PutUint64(p[0:], id)
	le.PutUint64(p[8:], 0)
	le.PutUint64(p[16:], ^uint64(0)) // len -1: todo el fichero
	_, _ = fs.dev.Write(notifyMsg(notifyInvalInode, p))
}

func (fs *fuseFS) notifyEntry(parent uint64, name string) {
	p := make([]byte, 16, 16+len(name)+1)
	le.PutUint64(p[0:], parent)
	le.PutUint32(p[8:], uint32(len(name)))
	p = append(append(p, name...), 0)
	_, _ = fs.dev.Write(notifyMsg(notifyInvalEntry, p))
}

func (fs *fuseFS) notifyDel(parent, child uint64, name string) {
	p := make([]byte, 24, 24+len(name)+1)
	le.PutUint64(p[0:], parent)
	le.PutUint64(p[8:], child)
	le.PutUint32(p[16:], uint32(len(name)))
	p = append(append(p, name...), 0)
	_, _ = fs.dev.Write(notifyMsg(notifyDelete, p))
}
