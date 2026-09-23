package guest

// El sistema de ficheros FUSE de una carpeta compartida en vivo: lee peticiones
// del kernel, las traduce a operaciones por ruta del protocolo kling-share y las
// manda al daemon por la sesión que esté viva.
//
// Lo que llega del kernel se valida igual que si viniera de fuera —tamaños,
// nombres, ids de nodo y de handle—: un mensaje mal formado no puede tumbar a
// PID 1. Lo que vale como frontera de seguridad, en cualquier caso, es lo que
// valida el daemon (internal/share): este código corre dentro del invitado.

import (
	"hash/fnv"
	"log"
	"sync"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/share"
)

// Límites del agente. Ver docs/compartir.md.
const (
	// maxOpen es el máximo de ficheros y directorios abiertos por carpeta.
	maxOpen = 4096
	// maxDirEntries es lo más que se lista de un directorio al abrirlo.
	maxDirEntries = 1 << 18
	// detachedGrace es cuánto espera una operación a que vuelva la sesión (tras
	// congelar, o mientras el daemon se reinicia) antes de fallar con EIO.
	detachedGrace = 30 * time.Second
)

// fuseReadBuf es el tamaño del búfer de lectura de /dev/fuse: el kernel exige
// que quepa la escritura más grande con su cabecera.
const fuseReadBuf = share.MaxIO + 8<<10

// fuseDev es /dev/fuse: cada Read devuelve UNA petición entera y cada Write
// manda UNA respuesta entera. En los tests es un falso en memoria.
type fuseDev interface {
	Read(buf []byte) (int, error)
	Write(msg []byte) (int, error)
}

type fileHandle struct {
	mu    sync.Mutex
	node  *node
	flags uint32 // del protocolo, sin FlagTrunc ni FlagExcl (sirven para reabrir)
	dh    uint64 // handle del daemon
	sess  *shareSession
}

type dirEntry struct {
	name string
	typ  uint32 // S_IF*
}

type dirHandle struct {
	node    *node
	entries []dirEntry
}

// fuseFS es un montaje. Sobrevive a las sesiones: al volver el daemon, sigue.
type fuseFS struct {
	dev      fuseDev
	mount    string
	readOnly bool

	smu   sync.Mutex
	sess  *shareSession
	ready chan struct{} // cerrado mientras hay sesión

	nodes *nodeTable

	hmu    sync.Mutex
	files  map[uint64]*fileHandle
	dirs   map[uint64]*dirHandle
	nextFH uint64

	wmu      sync.Mutex // serializa las respuestas en dispositivos que no lo hacen solos
	inflight chan struct{}
	grace    time.Duration
}

func newFuseFS(dev fuseDev, mount string, readOnly bool) *fuseFS {
	return &fuseFS{
		dev: dev, mount: mount, readOnly: readOnly,
		ready: make(chan struct{}),
		nodes: newNodeTable(),
		files: map[uint64]*fileHandle{}, dirs: map[uint64]*dirHandle{},
		inflight: make(chan struct{}, maxCalls),
		grace:    detachedGrace,
	}
}

// attach pone una sesión nueva y cierra la anterior. Lo que estuviera en vuelo
// en la vieja recibe EIO; lo que espere una sesión, sigue con esta.
func (fs *fuseFS) attach(s *shareSession) {
	fs.smu.Lock()
	old := fs.sess
	fs.sess = s
	select {
	case <-fs.ready:
	default:
		close(fs.ready)
	}
	fs.smu.Unlock()
	if old != nil {
		old.close()
	}
	go func() {
		<-s.done
		fs.dropSession(s)
	}()
}

// dropSession retira s si sigue siendo la sesión actual: quien espere una
// sesión espera a la siguiente.
func (fs *fuseFS) dropSession(s *shareSession) {
	fs.smu.Lock()
	defer fs.smu.Unlock()
	if fs.sess == s {
		fs.sess = nil
		fs.ready = make(chan struct{})
	}
}

// attached dice si hay sesión viva ahora mismo.
func (fs *fuseFS) attached() bool {
	fs.smu.Lock()
	defer fs.smu.Unlock()
	return fs.sess != nil && fs.sess.alive()
}

// session devuelve la sesión viva, esperando a que vuelva como mucho grace.
func (fs *fuseFS) session() *shareSession {
	deadline := time.Now().Add(fs.grace)
	for {
		fs.smu.Lock()
		s, ready := fs.sess, fs.ready
		fs.smu.Unlock()
		if s != nil {
			if s.alive() {
				return s
			}
			// Muerta pero aún puesta: se retira aquí, o se esperaría sobre un
			// ready ya cerrado dando vueltas hasta que lo hiciera su vigilante.
			fs.dropSession(s)
			continue
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil
		}
		t := time.NewTimer(left)
		select {
		case <-ready:
		case <-t.C:
		}
		t.Stop()
	}
}

// call manda una operación por la sesión viva.
func (fs *fuseFS) call(op byte, build func(*share.Enc)) (uint32, *share.Dec) {
	e, d, _ := fs.callS(op, build)
	return e, d
}

// callS es call devolviendo además la sesión que contestó (un handle abierto
// solo vale en ella). Si la operación no llegó a salir porque la sesión acababa
// de morir, espera a la siguiente y la repite.
func (fs *fuseFS) callS(op byte, build func(*share.Enc)) (uint32, *share.Dec, *shareSession) {
	for retries := 0; retries < maxRetries; retries++ {
		s := fs.session()
		if s == nil {
			return share.EIO, nil, nil
		}
		e, d := s.call(op, build)
		if e == errnoRetry {
			fs.dropSession(s)
			continue
		}
		return e, d, s
	}
	return share.EIO, nil, nil
}

// maxRetries acota las repeticiones de una operación que no llegó a salir: con
// sesiones que mueren nada más nacer, mejor EIO que un bucle.
const maxRetries = 8

// ── bucle ───────────────────────────────────────────────────────────────────

// serve lee peticiones del kernel hasta que se desmonta.
func (fs *fuseFS) serve() {
	buf := make([]byte, fuseReadBuf)
	for {
		n, err := fs.dev.Read(buf)
		if err != nil {
			if err == syscall.EINTR || err == syscall.EAGAIN {
				continue
			}
			// ENODEV: se desmontó. Cualquier otra cosa, tampoco hay más que leer.
			if err != syscall.ENODEV {
				log.Printf("share %s: reading /dev/fuse: %v", fs.mount, err)
			}
			return
		}
		msg := append([]byte(nil), buf[:n]...)
		req, err := parseReq(msg)
		if err != nil {
			log.Printf("share %s: %v", fs.mount, err)
			continue
		}
		switch req.opcode {
		case opInit:
			fs.write(fs.init(req))
			continue
		case opForget:
			if len(req.arg) >= 8 {
				fs.nodes.forget(req.nodeid, le.Uint64(req.arg))
			}
			continue
		case opBatchForget:
			fs.batchForget(req.arg)
			continue
		case opInterrupt:
			// Sin respuesta: la operación interrumpida contestará sola, con su
			// resultado o con EIO, y el kernel lo acepta en cualquier orden.
			continue
		case opDestroy:
			fs.write(reply(req.unique, 0))
			continue
		}
		fs.inflight <- struct{}{}
		go func() {
			defer func() { <-fs.inflight }()
			if out := fs.handle(req); out != nil {
				fs.write(out)
			}
		}()
	}
}

func (fs *fuseFS) write(b []byte) {
	fs.wmu.Lock()
	defer fs.wmu.Unlock()
	// ENOENT: el kernel ya no espera esta respuesta (la petición se abortó).
	// No hay nada que hacer con el error.
	_, _ = fs.dev.Write(b)
}

func (fs *fuseFS) init(req fuseReq) []byte {
	if len(req.arg) < 16 {
		return reply(req.unique, share.EINVAL)
	}
	major, minor := le.Uint32(req.arg[0:]), le.Uint32(req.arg[4:])
	readahead, flags := le.Uint32(req.arg[8:]), le.Uint32(req.arg[12:])
	out := make([]byte, initOutSize)
	le.PutUint32(out[0:], fuseMajor)
	if major < fuseMajor || (major == fuseMajor && minor < fuseMinMinor) {
		log.Printf("share %s: the kernel speaks FUSE %d.%d, too old", fs.mount, major, minor)
		return reply(req.unique, 71) // EPROTO
	}
	if major > fuseMajor {
		// El kernel reintenta con nuestra versión mayor.
		return reply(req.unique, 0, out[:8])
	}
	if minor > fuseMinor {
		minor = fuseMinor
	}
	if readahead > share.MaxIO {
		readahead = share.MaxIO
	}
	want := uint32(initAsyncRead | initAtomicOTrunc | initBigWrites | initAutoInvalData | initParallelDirop | initMaxPages)
	le.PutUint32(out[4:], minor)
	le.PutUint32(out[8:], readahead)
	le.PutUint32(out[12:], flags&want)
	le.PutUint16(out[16:], 16)               // max_background
	le.PutUint16(out[18:], 12)               // congestion_threshold
	le.PutUint32(out[20:], share.MaxIO)      // max_write
	le.PutUint32(out[24:], 1)                // time_gran: nanosegundos
	le.PutUint16(out[28:], share.MaxIO/4096) // max_pages
	return reply(req.unique, 0, out)
}

func (fs *fuseFS) batchForget(arg []byte) {
	if len(arg) < 8 {
		return
	}
	count := int(le.Uint32(arg))
	arg = arg[8:]
	if count > len(arg)/16 {
		count = len(arg) / 16
	}
	for i := 0; i < count; i++ {
		fs.nodes.forget(le.Uint64(arg[i*16:]), le.Uint64(arg[i*16+8:]))
	}
}

// ── operaciones ─────────────────────────────────────────────────────────────

// handle atiende una petición y devuelve su respuesta (nil si no lleva).
func (fs *fuseFS) handle(req fuseReq) []byte {
	u := req.unique
	fail := func(errno uint32) []byte { return reply(u, errno) }

	// Todo lo que sigue opera sobre un nodo que tiene que existir.
	n := fs.nodes.get(req.nodeid)
	if n == nil {
		return fail(share.ESTALE)
	}
	a := req.arg

	switch req.opcode {
	case opLookup:
		name, _, err := cstring(a)
		if err != nil || share.ValidName(name) != nil {
			return fail(nameErr(name, err))
		}
		p, e := fs.childPath(n, name)
		if e != 0 {
			return fail(e)
		}
		errno, d := fs.call(share.OpStat, func(e *share.Enc) { e.Str(p) })
		if errno != 0 {
			return fail(errno)
		}
		at := share.GetAttr(d)
		if d.Err() != nil {
			return fail(share.EIO)
		}
		c := fs.nodes.lookup(n, name)
		return reply(u, 0, entryOut(c.id, toFattr(at, c.id)))

	case opGetattr:
		if len(a) < 16 {
			return fail(share.EINVAL)
		}
		if le.Uint32(a[0:])&getattrFH != 0 {
			if h := fs.file(le.Uint64(a[8:])); h != nil {
				errno, d := fs.handleCall(h, share.OpFstat, nil)
				if errno == 0 {
					at := share.GetAttr(d)
					if d.Err() == nil {
						return reply(u, 0, attrOut(toFattr(at, n.id)))
					}
				}
			}
		}
		return fs.statReply(u, n)

	case opSetattr:
		return fs.setattr(u, n, a)

	case opReadlink:
		p, ok := fs.nodes.path(n)
		if !ok {
			return fail(share.ENOENT)
		}
		errno, d := fs.call(share.OpReadlink, func(e *share.Enc) { e.Str(p) })
		if errno != 0 {
			return fail(errno)
		}
		t := d.Str()
		if d.Err() != nil {
			return fail(share.EIO)
		}
		return reply(u, 0, []byte(t))

	case opSymlink, opLink, opMknod:
		// Un enlace creado por el invitado en el host es un arma contra lo que
		// el host haga luego con esa carpeta. Ver docs/compartir.md.
		return fail(share.EPERM)

	case opMkdir:
		if len(a) < 8 {
			return fail(share.EINVAL)
		}
		mode := le.Uint32(a[0:])
		name, _, err := cstring(a[8:])
		if err != nil || share.ValidName(name) != nil {
			return fail(nameErr(name, err))
		}
		if fs.readOnly {
			return fail(share.EROFS)
		}
		p, e := fs.childPath(n, name)
		if e != 0 {
			return fail(e)
		}
		errno, d := fs.call(share.OpMkdir, func(e *share.Enc) { e.Str(p); e.U32(mode & 0o7777) })
		if errno != 0 {
			return fail(errno)
		}
		at := share.GetAttr(d)
		if d.Err() != nil {
			return fail(share.EIO)
		}
		c := fs.nodes.lookup(n, name)
		return reply(u, 0, entryOut(c.id, toFattr(at, c.id)))

	case opUnlink, opRmdir:
		name, _, err := cstring(a)
		if err != nil || share.ValidName(name) != nil {
			return fail(nameErr(name, err))
		}
		if fs.readOnly {
			return fail(share.EROFS)
		}
		p, e := fs.childPath(n, name)
		if e != 0 {
			return fail(e)
		}
		op := share.OpUnlink
		if req.opcode == opRmdir {
			op = share.OpRmdir
		}
		if errno, _ := fs.call(op, func(e *share.Enc) { e.Str(p) }); errno != 0 {
			return fail(errno)
		}
		fs.nodes.detach(n, name)
		return fail(0)

	case opRename, opRename2:
		var flags uint32
		off := 8
		if len(a) < 8 {
			return fail(share.EINVAL)
		}
		newdir := le.Uint64(a[0:])
		if req.opcode == opRename2 {
			if len(a) < 16 {
				return fail(share.EINVAL)
			}
			flags, off = le.Uint32(a[8:]), 16
			// RENAME_NOREPLACE sí; EXCHANGE y WHITEOUT no.
			if flags&^1 != 0 {
				return fail(share.EINVAL)
			}
		}
		oldName, rest, err := cstring(a[off:])
		if err != nil || share.ValidName(oldName) != nil {
			return fail(nameErr(oldName, err))
		}
		newName, _, err := cstring(rest)
		if err != nil || share.ValidName(newName) != nil {
			return fail(nameErr(newName, err))
		}
		if fs.readOnly {
			return fail(share.EROFS)
		}
		np := fs.nodes.get(newdir)
		if np == nil {
			return fail(share.ESTALE)
		}
		op, e := fs.childPath(n, oldName)
		if e != 0 {
			return fail(e)
		}
		nw, e := fs.childPath(np, newName)
		if e != 0 {
			return fail(e)
		}
		errno, _ := fs.call(share.OpRename, func(e *share.Enc) { e.Str(op); e.Str(nw); e.U32(flags) })
		if errno != 0 {
			return fail(errno)
		}
		fs.nodes.rename(n, oldName, np, newName)
		return fail(0)

	case opOpen:
		if len(a) < 4 {
			return fail(share.EINVAL)
		}
		flags, e := protoFlags(le.Uint32(a[0:]))
		if e != 0 {
			return fail(e)
		}
		if fs.readOnly && flags&(share.FlagWrite|share.FlagTrunc) != 0 {
			return fail(share.EROFS)
		}
		p, ok := fs.nodes.path(n)
		if !ok {
			return fail(share.ENOENT)
		}
		errno, d, s := fs.callS(share.OpOpen, func(e *share.Enc) { e.Str(p); e.U32(flags) })
		if errno != 0 {
			return fail(errno)
		}
		dh := d.U64()
		if d.Err() != nil {
			return fail(share.EIO)
		}
		fh, e := fs.addFile(&fileHandle{node: n, flags: flags &^ share.FlagTrunc, dh: dh, sess: s})
		if e != 0 {
			s.call(share.OpRelease, func(e *share.Enc) { e.U64(dh) })
			return fail(e)
		}
		return reply(u, 0, openOut(fh))

	case opCreate:
		if len(a) < 16 {
			return fail(share.EINVAL)
		}
		flags, e := protoFlags(le.Uint32(a[0:]))
		if e != 0 {
			return fail(e)
		}
		mode := le.Uint32(a[4:])
		if le.Uint32(a[0:])&linuxOExcl != 0 {
			flags |= share.FlagExcl
		}
		name, _, err := cstring(a[16:])
		if err != nil || share.ValidName(name) != nil {
			return fail(nameErr(name, err))
		}
		if fs.readOnly {
			return fail(share.EROFS)
		}
		p, e := fs.childPath(n, name)
		if e != 0 {
			return fail(e)
		}
		errno, d, s := fs.callS(share.OpCreate, func(e *share.Enc) { e.Str(p); e.U32(flags); e.U32(mode & 0o7777) })
		if errno != 0 {
			return fail(errno)
		}
		dh := d.U64()
		at := share.GetAttr(d)
		if d.Err() != nil {
			return fail(share.EIO)
		}
		c := fs.nodes.lookup(n, name)
		fh, e := fs.addFile(&fileHandle{node: c, flags: flags &^ (share.FlagTrunc | share.FlagExcl), dh: dh, sess: s})
		if e != 0 {
			s.call(share.OpRelease, func(e *share.Enc) { e.U64(dh) })
			fs.nodes.forget(c.id, 1)
			return fail(e)
		}
		return reply(u, 0, entryOut(c.id, toFattr(at, c.id)), openOut(fh))

	case opRead:
		if len(a) < 24 {
			return fail(share.EINVAL)
		}
		fh, off, size := le.Uint64(a[0:]), le.Uint64(a[8:]), le.Uint32(a[16:])
		if size > share.MaxIO || int64(off) < 0 {
			return fail(share.EINVAL)
		}
		h := fs.file(fh)
		if h == nil {
			return fail(share.EBADF)
		}
		errno, d := fs.handleCall(h, share.OpRead, func(e *share.Enc) { e.U64(off); e.U32(size) })
		if errno != 0 {
			return fail(errno)
		}
		data := d.Bytes(int(size))
		if d.Err() != nil {
			return fail(share.EIO)
		}
		return reply(u, 0, data)

	case opWrite:
		if len(a) < 40 {
			return fail(share.EINVAL)
		}
		fh, off, size := le.Uint64(a[0:]), le.Uint64(a[8:]), le.Uint32(a[16:])
		data := a[40:]
		if size > share.MaxIO || int(size) != len(data) || int64(off) < 0 {
			return fail(share.EINVAL)
		}
		if fs.readOnly {
			return fail(share.EROFS)
		}
		h := fs.file(fh)
		if h == nil {
			return fail(share.EBADF)
		}
		errno, d := fs.handleCall(h, share.OpWrite, func(e *share.Enc) { e.U64(off); e.Bytes(data) })
		if errno != 0 {
			return fail(errno)
		}
		w := d.U32()
		if d.Err() != nil || w > size {
			return fail(share.EIO)
		}
		out := make([]byte, writeOutSize)
		le.PutUint32(out, w)
		return reply(u, 0, out)

	case opStatfs:
		errno, d := fs.call(share.OpStatfs, nil)
		if errno != 0 {
			return fail(errno)
		}
		out := make([]byte, kstatfsSize)
		for i := 0; i < 5; i++ {
			le.PutUint64(out[i*8:], d.U64())
		}
		bsize, namelen := d.U32(), d.U32()
		if d.Err() != nil {
			return fail(share.EIO)
		}
		le.PutUint32(out[40:], bsize)
		le.PutUint32(out[44:], namelen)
		le.PutUint32(out[48:], bsize) // frsize
		return reply(u, 0, out)

	case opRelease:
		if len(a) < 8 {
			return fail(share.EINVAL)
		}
		if h := fs.dropFile(le.Uint64(a[0:])); h != nil {
			h.mu.Lock()
			s, dh := h.sess, h.dh
			h.mu.Unlock()
			// Solo si el handle es de la sesión viva: los de una sesión muerta
			// ya los cerró el daemon al cortarse.
			if s != nil && s.alive() {
				s.call(share.OpRelease, func(e *share.Enc) { e.U64(dh) })
			}
		}
		return fail(0)

	case opFsync:
		if len(a) < 8 {
			return fail(share.EINVAL)
		}
		h := fs.file(le.Uint64(a[0:]))
		if h == nil {
			return fail(share.EBADF)
		}
		errno, _ := fs.handleCall(h, share.OpFsync, nil)
		return fail(errno)

	case opFlush:
		// Cada write ya llegó al host antes de contestar: no hay nada que vaciar.
		return fail(0)

	case opOpendir:
		return fs.opendir(u, n)

	case opReaddir:
		if len(a) < 24 {
			return fail(share.EINVAL)
		}
		return fs.readdir(u, n, le.Uint64(a[0:]), le.Uint64(a[8:]), le.Uint32(a[16:]))

	case opReleasedir:
		if len(a) < 8 {
			return fail(share.EINVAL)
		}
		fs.hmu.Lock()
		delete(fs.dirs, le.Uint64(a[0:]))
		fs.hmu.Unlock()
		return fail(0)

	case opFsyncdir:
		return fail(0)

	case opAccess:
		if len(a) < 4 {
			return fail(share.EINVAL)
		}
		// Con default_permissions el kernel comprueba los permisos por su
		// cuenta; aquí solo queda decir si existe y si se puede escribir.
		if fs.readOnly && le.Uint32(a[0:])&2 != 0 {
			return fail(share.EROFS)
		}
		if out := fs.statReply(u, n); le.Uint32(out[4:]) != 0 {
			return out
		}
		return fail(0)
	}
	return fail(share.ENOSYS)
}

// nameErr decide el error de un nombre inválido.
func nameErr(name string, err error) uint32 {
	if err == nil && len(name) > share.MaxName {
		return share.ENAMETOOLONG
	}
	return share.EINVAL
}

// childPath es la ruta de parent/name, o ENOENT si parent ya no tiene ruta.
func (fs *fuseFS) childPath(parent *node, name string) (string, uint32) {
	p, ok := fs.nodes.path(parent)
	if !ok {
		return "", share.ENOENT
	}
	full := share.Join(p, name)
	if len(full) > share.MaxPath {
		return "", share.ENAMETOOLONG
	}
	return full, 0
}

func (fs *fuseFS) statReply(u uint64, n *node) []byte {
	p, ok := fs.nodes.path(n)
	if !ok {
		return reply(u, share.ENOENT)
	}
	errno, d := fs.call(share.OpStat, func(e *share.Enc) { e.Str(p) })
	if errno != 0 {
		return reply(u, errno)
	}
	at := share.GetAttr(d)
	if d.Err() != nil {
		return reply(u, share.EIO)
	}
	return reply(u, 0, attrOut(toFattr(at, n.id)))
}

func (fs *fuseFS) setattr(u uint64, n *node, a []byte) []byte {
	if len(a) < 88 {
		return reply(u, share.EINVAL)
	}
	valid := le.Uint32(a[0:])
	fh := le.Uint64(a[8:])
	size := le.Uint64(a[16:])
	atime, mtime := int64(le.Uint64(a[32:])), int64(le.Uint64(a[40:]))
	atimens, mtimens := le.Uint32(a[56:]), le.Uint32(a[60:])
	mode := le.Uint32(a[68:])
	if int64(size) < 0 || atimens >= 1e9 || mtimens >= 1e9 {
		return reply(u, share.EINVAL)
	}

	var v uint32
	if valid&fattrMode != 0 {
		v |= share.SetMode
	}
	if valid&fattrSize != 0 {
		v |= share.SetSize
	}
	switch {
	case valid&fattrAtimeNow != 0:
		v |= share.SetAtimeNow
	case valid&fattrAtime != 0:
		v |= share.SetAtime
	}
	switch {
	case valid&fattrMtimeNow != 0:
		v |= share.SetMtimeNow
	case valid&fattrMtime != 0:
		v |= share.SetMtime
	}
	// uid/gid no hacen nada: para el invitado todo es de root, y el dueño en
	// el host no lo decide él. ctime la pone el host solo.
	if v == 0 {
		return fs.statReply(u, n)
	}
	if fs.readOnly {
		return reply(u, share.EROFS)
	}
	p, ok := fs.nodes.path(n)
	var h *fileHandle
	if valid&fattrFH != 0 {
		h = fs.file(fh)
	}
	if !ok && h == nil {
		return reply(u, share.ENOENT)
	}
	build := func(dh uint64) func(*share.Enc) {
		return func(e *share.Enc) {
			e.Str(p)
			e.U64(dh)
			e.U32(v)
			e.U32(mode & 0o7777)
			e.U64(size)
			e.I64(atime*1e9 + int64(atimens))
			e.I64(mtime*1e9 + int64(mtimens))
		}
	}
	var errno uint32
	var d *share.Dec
	if h != nil {
		errno, d = fs.handleCallRaw(h, func(s *shareSession, dh uint64) (uint32, *share.Dec) {
			return s.call(share.OpSetattr, build(dh))
		})
	} else {
		errno, d = fs.call(share.OpSetattr, build(0))
	}
	if errno != 0 {
		return reply(u, errno)
	}
	at := share.GetAttr(d)
	if d.Err() != nil {
		return reply(u, share.EIO)
	}
	return reply(u, 0, attrOut(toFattr(at, n.id)))
}

func (fs *fuseFS) opendir(u uint64, n *node) []byte {
	p, ok := fs.nodes.path(n)
	if !ok {
		return reply(u, share.ENOENT)
	}
	entries := []dirEntry{{".", share.SIFDIR}, {"..", share.SIFDIR}}
	var cookie uint64
	for {
		errno, d := fs.call(share.OpList, func(e *share.Enc) { e.Str(p); e.U64(cookie); e.U32(64 << 10) })
		if errno != 0 {
			return reply(u, errno)
		}
		eof, count := d.U8(), d.U32()
		for i := uint32(0); i < count && d.Err() == nil; i++ {
			name, typ := d.Str(), d.U32()
			if d.Err() == nil && share.ValidName(name) == nil {
				entries = append(entries, dirEntry{name, typ & share.SIFMT})
			}
		}
		next := d.U64()
		if d.Err() != nil {
			return reply(u, share.EIO)
		}
		if len(entries) > maxDirEntries {
			return reply(u, share.EFBIG)
		}
		if eof != 0 {
			break
		}
		if next <= cookie {
			// El daemon no avanza: mejor un error que un bucle.
			return reply(u, share.EIO)
		}
		cookie = next
	}
	fs.hmu.Lock()
	defer fs.hmu.Unlock()
	if len(fs.files)+len(fs.dirs) >= maxOpen {
		return reply(u, share.EMFILE)
	}
	fs.nextFH++
	fs.dirs[fs.nextFH] = &dirHandle{node: n, entries: entries}
	return reply(u, 0, openOut(fs.nextFH))
}

func (fs *fuseFS) readdir(u uint64, n *node, fh, off uint64, size uint32) []byte {
	fs.hmu.Lock()
	dh := fs.dirs[fh]
	fs.hmu.Unlock()
	if dh == nil {
		return reply(u, share.EBADF)
	}
	if size > share.MaxIO {
		size = share.MaxIO
	}
	var buf []byte
	for i := off; i < uint64(len(dh.entries)); i++ {
		e := dh.entries[i]
		var ok bool
		buf, ok = appendDirent(buf, int(size), fs.direntIno(dh.node, e.name), i+1, e.typ>>12, e.name)
		if !ok {
			break
		}
	}
	return reply(u, 0, buf)
}

// direntIno es el número de inodo de una entrada de READDIR. El del nodo si el
// kernel ya lo conoce; si no, uno sintético con el bit alto puesto (los ids
// reales son un contador y nunca llegan ahí). Nunca 0: la libc se salta las
// entradas con d_ino 0 como si estuvieran borradas.
func (fs *fuseFS) direntIno(dir *node, name string) uint64 {
	switch name {
	case ".":
		return dir.id
	case "..":
		return fs.nodes.parentID(dir)
	}
	if c := fs.nodes.child(dir, name); c != nil {
		return c.id
	}
	h := fnv.New64a()
	var b [8]byte
	le.PutUint64(b[:], dir.id)
	_, _ = h.Write(b[:])
	_, _ = h.Write([]byte(name))
	return h.Sum64() | 1<<63
}

// ── handles ─────────────────────────────────────────────────────────────────

func (fs *fuseFS) addFile(h *fileHandle) (uint64, uint32) {
	fs.hmu.Lock()
	defer fs.hmu.Unlock()
	if len(fs.files)+len(fs.dirs) >= maxOpen {
		return 0, share.EMFILE
	}
	fs.nextFH++
	fs.files[fs.nextFH] = h
	return fs.nextFH, 0
}

func (fs *fuseFS) file(fh uint64) *fileHandle {
	fs.hmu.Lock()
	defer fs.hmu.Unlock()
	return fs.files[fh]
}

func (fs *fuseFS) dropFile(fh uint64) *fileHandle {
	fs.hmu.Lock()
	defer fs.hmu.Unlock()
	h := fs.files[fh]
	delete(fs.files, fh)
	return h
}

// handleCall hace una operación sobre un fichero abierto. Si el handle es de
// una sesión anterior (se congeló la máquina, se reinició el daemon), o el
// daemon no lo conoce, se reabre por ruta y se reintenta una vez.
func (fs *fuseFS) handleCall(h *fileHandle, op byte, args func(*share.Enc)) (uint32, *share.Dec) {
	return fs.handleCallRaw(h, func(s *shareSession, dh uint64) (uint32, *share.Dec) {
		return s.call(op, func(e *share.Enc) {
			e.U64(dh)
			if args != nil {
				args(e)
			}
		})
	})
}

func (fs *fuseFS) handleCallRaw(h *fileHandle, do func(*shareSession, uint64) (uint32, *share.Dec)) (uint32, *share.Dec) {
	for attempt, retries := 0, 0; attempt < 2 && retries < maxRetries; retries++ {
		s := fs.session()
		if s == nil {
			return share.EIO, nil
		}
		h.mu.Lock()
		if h.sess != s {
			if e := fs.reopenLocked(h, s); e != 0 {
				h.mu.Unlock()
				if e == errnoRetry {
					fs.dropSession(s)
					continue
				}
				return e, nil
			}
		}
		dh := h.dh
		h.mu.Unlock()
		errno, d := do(s, dh)
		if errno == errnoRetry {
			// No salió: la sesión acababa de morir. Con la siguiente, y sin
			// gastar intento.
			fs.dropSession(s)
			continue
		}
		attempt++
		if errno != share.ESTALE {
			return errno, d
		}
		// El daemon no conoce el handle: marcarlo para reabrir en la vuelta.
		h.mu.Lock()
		if h.dh == dh {
			h.sess = nil
		}
		h.mu.Unlock()
	}
	return share.ESTALE, nil
}

func (fs *fuseFS) reopenLocked(h *fileHandle, s *shareSession) uint32 {
	p, ok := fs.nodes.path(h.node)
	if !ok {
		// Se borró mientras estaba abierto y el host ya no lo tiene: sus datos
		// se fueron con la sesión anterior.
		return share.ESTALE
	}
	errno, d := s.call(share.OpOpen, func(e *share.Enc) { e.Str(p); e.U32(h.flags) })
	if errno != 0 {
		return errno
	}
	dh := d.U64()
	if d.Err() != nil {
		return share.EIO
	}
	h.dh, h.sess = dh, s
	return 0
}

// ── traducciones ────────────────────────────────────────────────────────────

// protoFlags traduce los flags de open de Linux a los del protocolo.
func protoFlags(f uint32) (uint32, uint32) {
	var out uint32
	switch f & linuxOAccMode {
	case linuxORdonly:
		out = share.FlagRead
	case linuxOWronly:
		out = share.FlagWrite
	case linuxORdwr:
		out = share.FlagRead | share.FlagWrite
	default:
		return 0, share.EINVAL
	}
	if f&linuxOTrunc != 0 {
		out |= share.FlagTrunc
	}
	if f&linuxOAppend != 0 {
		out |= share.FlagAppend
	}
	return out, 0
}

// toFattr pasa los atributos del protocolo a los de FUSE. El inodo es el id del
// nodo (el número de inodo del host no sale de él), y todo es de root.
func toFattr(a share.Attr, ino uint64) fattr {
	return fattr{
		ino: ino, size: a.Size, blocks: a.Blocks,
		atime: uint64(a.Atime), mtime: uint64(a.Mtime), ctime: uint64(a.Ctime),
		atimensec: a.Atimen, mtimensec: a.Mtimen, ctimns: a.Ctimen,
		mode: a.Mode, nlink: a.Nlink, blksize: 4096,
	}
}
