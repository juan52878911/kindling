// Package share sirve las carpetas compartidas en vivo desde el host y valida
// las subidas del modo copy. Ver docs/compartir.md.
//
// Lo que llega aquí lo manda el agente de invitado, que corre dentro de una
// microVM hostil: cada ruta, tamaño, offset y handle se valida, y todo acceso al
// disco pasa por os.Root, que no deja salir de la carpeta ni con ".." ni con un
// enlace simbólico.
package share

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	proto "github.com/juan52878911/kindling/pkg/share"
)

// Límites del lado del host. Ver docs/compartir.md.
const (
	// maxHandles por sesión: un invitado que abre sin cerrar se queda sin
	// handles él, no deja al daemon sin descriptores.
	maxHandles = 1024
	// maxInflight por sesión: operaciones a la vez. La siguiente trama no se
	// lee hasta que hay hueco, así que la contrapresión llega al invitado por
	// TCP en vez de acumularse aquí.
	maxInflight = 16
	// maxListBytes es lo más que ocupa una página de listado.
	maxListBytes = 64 << 10
)

// globalFDs acota los descriptores abiertos por TODAS las sesiones del daemon.
// Con 256 máquinas y 8 carpetas cada una, el tope por sesión solo no bastaría.
var globalFDs = make(chan struct{}, 16384)

// Server sirve una carpeta del host. Uno por carpeta de cada máquina; cada
// conexión del agente es una sesión nueva sobre él.
type Server struct {
	dir      string
	root     *os.Root
	readOnly bool

	// uid/gid del dueño de la carpeta, a quien se entrega lo que crea el
	// invitado si el daemon corre como root. -1 = no se toca.
	uid, gid int
}

// Open abre la carpeta dir para servirla.
func Open(dir string, readOnly bool) (*Server, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	s := &Server{dir: dir, root: root, readOnly: readOnly, uid: -1, gid: -1}
	if os.Geteuid() == 0 {
		// Si el daemon es root, lo que cree el invitado sería de root en el
		// host y su dueño no podría ni borrarlo. Se entrega al dueño de la
		// carpeta, que es quien la compartió.
		if fi, err := root.Stat("."); err == nil {
			if u, g, ok := owner(fi); ok {
				s.uid, s.gid = u, g
			}
		}
	}
	return s, nil
}

// Close suelta la carpeta.
func (s *Server) Close() error { return s.root.Close() }

// session es el estado de UNA conexión: sus ficheros abiertos. Muere con ella.
type session struct {
	srv   *Server
	epoch uint64
	mu    sync.Mutex
	next  uint64
	files map[uint64]*handle
}

type handle struct {
	f     *os.File
	write bool
}

// Serve atiende una conexión del agente hasta que se corta o ctx se cancela.
// Al volver, los ficheros que el invitado tuviera abiertos se cierran: si vuelve
// a conectar, los reabre por ruta (ver ESTALE en el agente).
func (s *Server) Serve(ctx context.Context, conn io.ReadWriteCloser) error {
	var b [4]byte
	_, _ = rand.Read(b[:])
	sess := &session{srv: s, epoch: uint64(binary.BigEndian.Uint32(b[:])|1) << 32, files: map[uint64]*handle{}}
	defer sess.closeAll()

	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	var wmu sync.Mutex
	slots := make(chan struct{}, maxInflight)
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		body, err := proto.ReadFrame(conn)
		if err != nil {
			_ = conn.Close()
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		d := proto.NewDec(body)
		id, op := d.U64(), d.U8()
		if d.Err() != nil {
			_ = conn.Close()
			return errors.New("the guest sent a message without a header")
		}
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-slots; wg.Done() }()
			resp := sess.dispatch(id, op, d)
			wmu.Lock()
			err := proto.WriteFrame(conn, resp)
			wmu.Unlock()
			if err != nil {
				_ = conn.Close()
			}
		}()
	}
}

func (ss *session) closeAll() {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for id, h := range ss.files {
		_ = h.f.Close()
		<-globalFDs
		delete(ss.files, id)
	}
}

// errReply es una respuesta de error.
func errReply(id uint64, errno uint32) []byte { return proto.Response(id, errno).B }

// dispatch ejecuta una operación y devuelve la respuesta ya codificada.
func (ss *session) dispatch(id uint64, op byte, d *proto.Dec) (out []byte) {
	defer func() {
		// Un pánico en una operación no se lleva el daemon —y con él todas las
		// microVMs—: se contesta EIO y se deja constancia.
		if r := recover(); r != nil {
			log.Printf("share: panic serving op %d: %v", op, r)
			out = errReply(id, proto.EIO)
		}
	}()
	s := ss.srv
	ok := func() *proto.Enc { return proto.Response(id, 0) }

	switch op {
	case proto.OpStat:
		p := d.Str()
		if e := checked(d, p); e != 0 {
			return errReply(id, e)
		}
		a, e := s.stat(p)
		if e != 0 {
			return errReply(id, e)
		}
		r := ok()
		a.Put(r)
		return r.B

	case proto.OpFstat:
		h := d.U64()
		if d.Done() != nil {
			return errReply(id, proto.EINVAL)
		}
		hd := ss.get(h)
		if hd == nil {
			return errReply(id, proto.ESTALE)
		}
		fi, err := hd.f.Stat()
		if err != nil {
			return errReply(id, Errno(err))
		}
		r := ok()
		attrOf(fi).Put(r)
		return r.B

	case proto.OpList:
		p, cookie, max := d.Str(), d.U64(), d.U32()
		if e := checked(d, p); e != 0 {
			return errReply(id, e)
		}
		if max == 0 || max > maxListBytes {
			max = maxListBytes
		}
		return s.list(id, p, cookie, int(max))

	case proto.OpOpen, proto.OpCreate:
		p, flags := d.Str(), d.U32()
		var mode uint32
		if op == proto.OpCreate {
			mode = d.U32()
		}
		if e := checked(d, p); e != 0 {
			return errReply(id, e)
		}
		if p == "" {
			return errReply(id, proto.EISDIR)
		}
		f, write, e := s.open(p, flags, op == proto.OpCreate, mode)
		if e != 0 {
			return errReply(id, e)
		}
		fi, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return errReply(id, Errno(err))
		}
		h, e := ss.put(f, write)
		if e != 0 {
			return errReply(id, e)
		}
		r := ok()
		r.U64(h)
		attrOf(fi).Put(r)
		return r.B

	case proto.OpRead:
		h, off, size := d.U64(), d.U64(), d.U32()
		if d.Done() != nil || size > proto.MaxIO || int64(off) < 0 || int64(off)+int64(size) < 0 {
			return errReply(id, proto.EINVAL)
		}
		hd := ss.get(h)
		if hd == nil {
			return errReply(id, proto.ESTALE)
		}
		buf := make([]byte, size)
		n, err := hd.f.ReadAt(buf, int64(off))
		if err != nil && !errors.Is(err, io.EOF) {
			return errReply(id, Errno(err))
		}
		r := ok()
		r.Bytes(buf[:n])
		return r.B

	case proto.OpWrite:
		h, off, data := d.U64(), d.U64(), d.Bytes(proto.MaxIO)
		if d.Done() != nil || int64(off) < 0 || int64(off)+int64(len(data)) < 0 {
			return errReply(id, proto.EINVAL)
		}
		if s.readOnly {
			return errReply(id, proto.EROFS)
		}
		hd := ss.get(h)
		if hd == nil {
			return errReply(id, proto.ESTALE)
		}
		if !hd.write {
			return errReply(id, proto.EBADF)
		}
		n, err := hd.f.WriteAt(data, int64(off))
		if err != nil && n == 0 {
			return errReply(id, Errno(err))
		}
		r := ok()
		r.U32(uint32(n))
		return r.B

	case proto.OpRelease:
		h := d.U64()
		if d.Done() != nil {
			return errReply(id, proto.EINVAL)
		}
		ss.release(h)
		return ok().B

	case proto.OpFsync:
		h := d.U64()
		if d.Done() != nil {
			return errReply(id, proto.EINVAL)
		}
		hd := ss.get(h)
		if hd == nil {
			return errReply(id, proto.ESTALE)
		}
		if hd.write {
			if err := hd.f.Sync(); err != nil {
				return errReply(id, Errno(err))
			}
		}
		return ok().B

	case proto.OpSetattr:
		p, h, valid, mode, size, at, mt := d.Str(), d.U64(), d.U32(), d.U32(), d.U64(), d.I64(), d.I64()
		if e := checked(d, p); e != 0 {
			return errReply(id, e)
		}
		if int64(size) < 0 {
			return errReply(id, proto.EINVAL)
		}
		var hf *os.File
		if h != 0 {
			if hd := ss.get(h); hd != nil {
				hf = hd.f
			}
		}
		a, e := s.setattr(p, hf, valid, mode, int64(size), at, mt)
		if e != 0 {
			return errReply(id, e)
		}
		r := ok()
		a.Put(r)
		return r.B

	case proto.OpMkdir:
		p, mode := d.Str(), d.U32()
		if e := checked(d, p); e != 0 {
			return errReply(id, e)
		}
		a, e := s.mkdir(p, mode)
		if e != 0 {
			return errReply(id, e)
		}
		r := ok()
		a.Put(r)
		return r.B

	case proto.OpUnlink, proto.OpRmdir:
		p := d.Str()
		if e := checked(d, p); e != 0 {
			return errReply(id, e)
		}
		if e := s.remove(p, op == proto.OpRmdir); e != 0 {
			return errReply(id, e)
		}
		return ok().B

	case proto.OpRename:
		oldp, newp, flags := d.Str(), d.Str(), d.U32()
		if d.Done() != nil {
			return errReply(id, proto.EINVAL)
		}
		if proto.ValidPath(oldp) != nil || proto.ValidPath(newp) != nil {
			return errReply(id, proto.EINVAL)
		}
		if e := s.rename(oldp, newp, flags); e != 0 {
			return errReply(id, e)
		}
		return ok().B

	case proto.OpReadlink:
		p := d.Str()
		if e := checked(d, p); e != 0 {
			return errReply(id, e)
		}
		t, e := s.readlink(p)
		if e != 0 {
			return errReply(id, e)
		}
		r := ok()
		r.Str(t)
		return r.B

	case proto.OpStatfs:
		if d.Done() != nil {
			return errReply(id, proto.EINVAL)
		}
		st, e := s.statfs()
		if e != 0 {
			return errReply(id, e)
		}
		r := ok()
		r.U64(st.Blocks)
		r.U64(st.Bfree)
		r.U64(st.Bavail)
		r.U64(st.Files)
		r.U64(st.Ffree)
		r.U32(st.Bsize)
		r.U32(st.Namelen)
		return r.B
	}
	return errReply(id, proto.ENOSYS)
}

// checked termina de decodificar una petición cuyo primer argumento es una
// ruta y la valida.
func checked(d *proto.Dec, p string) uint32 {
	if d.Done() != nil {
		return proto.EINVAL
	}
	if err := proto.ValidPath(p); err != nil {
		if len(p) > proto.MaxPath {
			return proto.ENAMETOOLONG
		}
		return proto.EINVAL
	}
	return 0
}

func (ss *session) get(h uint64) *handle {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.files[h]
}

func (ss *session) put(f *os.File, write bool) (uint64, uint32) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if len(ss.files) >= maxHandles {
		_ = f.Close()
		return 0, proto.EMFILE
	}
	select {
	case globalFDs <- struct{}{}:
	default:
		_ = f.Close()
		return 0, proto.EMFILE
	}
	ss.next++
	// La época en la mitad alta: un handle de una sesión anterior (de antes de
	// congelar, o de otro proceso del daemon) no puede coincidir con uno de esta.
	h := ss.epoch | (ss.next & 0xffffffff)
	ss.files[h] = &handle{f: f, write: write}
	return h, 0
}

func (ss *session) release(h uint64) {
	ss.mu.Lock()
	hd := ss.files[h]
	delete(ss.files, h)
	ss.mu.Unlock()
	if hd != nil {
		_ = hd.f.Close()
		<-globalFDs
	}
}

// ── operaciones sobre el disco ───────────────────────────────────────────────

func rel(p string) string {
	if p == "" {
		return "."
	}
	return p
}

// servible dice si un tipo de fichero del host se enseña al invitado. Los
// dispositivos, FIFOs y sockets no: abrir un dispositivo del host desde el
// invitado es justo lo que una carpeta compartida no debe permitir.
func servible(m fs.FileMode) bool {
	t := m.Type()
	return t == 0 || t == fs.ModeDir || t == fs.ModeSymlink
}

func (s *Server) stat(p string) (proto.Attr, uint32) {
	fi, err := s.root.Lstat(rel(p))
	if err != nil {
		return proto.Attr{}, Errno(err)
	}
	if !servible(fi.Mode()) {
		return proto.Attr{}, proto.ENOENT
	}
	return attrOf(fi), 0
}

func (s *Server) list(id uint64, p string, cookie uint64, max int) []byte {
	f, err := s.root.Open(rel(p))
	if err != nil {
		return errReply(id, Errno(err))
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return errReply(id, Errno(err))
	} else if !fi.IsDir() {
		return errReply(id, proto.ENOTDIR)
	}

	var ent proto.Enc
	n, idx, eof := uint32(0), uint64(0), false
	for {
		batch, err := f.ReadDir(256)
		for _, de := range batch {
			if idx < cookie {
				idx++
				continue
			}
			if !servible(de.Type()) || proto.ValidName(de.Name()) != nil {
				idx++
				continue
			}
			if len(ent.B)+len(de.Name())+6 > max {
				goto done
			}
			ent.Str(de.Name())
			ent.U32(typeBits(de.Type()))
			n++
			idx++
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return errReply(id, Errno(err))
			}
			eof = true
			break
		}
		if len(batch) == 0 {
			eof = true
			break
		}
	}
done:
	r := proto.Response(id, 0)
	if eof {
		r.U8(1)
	} else {
		r.U8(0)
	}
	r.U32(n)
	r.B = append(r.B, ent.B...)
	r.U64(idx)
	return r.B
}

func (s *Server) open(p string, flags uint32, create bool, mode uint32) (*os.File, bool, uint32) {
	write := flags&proto.FlagWrite != 0
	if s.readOnly && (write || create || flags&(proto.FlagTrunc|proto.FlagAppend) != 0) {
		return nil, false, proto.EROFS
	}
	var of int
	switch {
	case flags&proto.FlagRead != 0 && write:
		of = os.O_RDWR
	case write:
		of = os.O_WRONLY
	default:
		of = os.O_RDONLY
	}
	if flags&proto.FlagTrunc != 0 {
		if !write {
			return nil, false, proto.EINVAL
		}
		of |= os.O_TRUNC
	}
	// O_APPEND no pasa al host: el kernel del invitado ya manda cada escritura
	// con su offset, y con O_APPEND Linux ignoraría ese offset en pwrite.
	if create {
		of |= os.O_CREATE
		if flags&proto.FlagExcl != 0 {
			of |= os.O_EXCL
		}
	} else {
		// Antes de abrir: abrir un FIFO bloquea, y abrir un dispositivo ya es
		// hacerle algo. Solo se abre lo que es un fichero regular.
		fi, err := s.root.Lstat(p)
		if err != nil {
			return nil, false, Errno(err)
		}
		switch {
		case fi.IsDir():
			return nil, false, proto.EISDIR
		case fi.Mode()&fs.ModeSymlink != 0:
			return nil, false, proto.ELOOP
		case !fi.Mode().IsRegular():
			return nil, false, proto.EACCES
		}
	}
	// O_NONBLOCK por si entre la comprobación y la apertura alguien cambió el
	// fichero por un FIFO: no bloquea al daemon, y la comprobación de después lo
	// descarta.
	f, err := s.root.OpenFile(p, of|syscall.O_NONBLOCK, fs.FileMode(mode&0o777))
	if err != nil {
		return nil, false, Errno(err)
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		if err != nil {
			return nil, false, Errno(err)
		}
		return nil, false, proto.EACCES
	}
	if create {
		s.chown(f)
		// El modo pedido, recortado: sin setuid ni setgid, aunque el umask del
		// daemon hubiera dejado otra cosa.
		_ = f.Chmod(fs.FileMode(mode & 0o777))
	}
	return f, write, 0
}

// chown entrega al dueño de la carpeta lo que acaba de crear el invitado.
func (s *Server) chown(f *os.File) {
	if s.uid >= 0 {
		_ = f.Chown(s.uid, s.gid)
	}
}

func (s *Server) setattr(p string, hf *os.File, valid, mode uint32, size, at, mt int64) (proto.Attr, uint32) {
	if s.readOnly && valid != 0 {
		return proto.Attr{}, proto.EROFS
	}
	f := hf
	if f == nil {
		fi, err := s.root.Lstat(rel(p))
		if err != nil {
			return proto.Attr{}, Errno(err)
		}
		if !servible(fi.Mode()) {
			return proto.Attr{}, proto.ENOENT
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			// Un enlace no tiene modo propio en Linux, y cambiarle el tamaño no
			// tiene sentido. Sus fechas se dejan: no merece un *at propio.
			if valid&(proto.SetMode|proto.SetSize) != 0 {
				return proto.Attr{}, proto.EPERM
			}
			return attrOf(fi), 0
		}
		flag := os.O_RDONLY
		if valid&proto.SetSize != 0 {
			if fi.IsDir() {
				return proto.Attr{}, proto.EISDIR
			}
			flag = os.O_WRONLY
		}
		opened, err := s.root.OpenFile(rel(p), flag|syscall.O_NONBLOCK, 0)
		if err != nil && flag == os.O_RDONLY && !fi.IsDir() {
			// Un fichero de solo escritura (0200) no se abre para leer si el
			// daemon no es root; para cambiarle el modo o las fechas vale igual.
			opened, err = s.root.OpenFile(rel(p), os.O_WRONLY|syscall.O_NONBLOCK, 0)
		}
		if err != nil {
			return proto.Attr{}, Errno(err)
		}
		defer opened.Close()
		if st, err := opened.Stat(); err != nil || !servible(st.Mode()) || st.Mode()&fs.ModeSymlink != 0 {
			return proto.Attr{}, proto.EACCES
		}
		f = opened
	}
	if valid&proto.SetSize != 0 {
		if err := f.Truncate(size); err != nil {
			return proto.Attr{}, Errno(err)
		}
	}
	if valid&proto.SetMode != 0 {
		if err := f.Chmod(fs.FileMode(mode & 0o777)); err != nil {
			return proto.Attr{}, Errno(err)
		}
	}
	if valid&(proto.SetAtime|proto.SetMtime|proto.SetAtimeNow|proto.SetMtimeNow) != 0 {
		fi, err := f.Stat()
		if err != nil {
			return proto.Attr{}, Errno(err)
		}
		cur := attrOf(fi)
		now := time.Now().UnixNano()
		a := cur.Atime*1e9 + int64(cur.Atimen)
		m := cur.Mtime*1e9 + int64(cur.Mtimen)
		switch {
		case valid&proto.SetAtimeNow != 0:
			a = now
		case valid&proto.SetAtime != 0:
			a = at
		}
		switch {
		case valid&proto.SetMtimeNow != 0:
			m = now
		case valid&proto.SetMtime != 0:
			m = mt
		}
		tv := []syscall.Timeval{syscall.NsecToTimeval(a), syscall.NsecToTimeval(m)}
		if err := syscall.Futimes(int(f.Fd()), tv); err != nil {
			return proto.Attr{}, Errno(err)
		}
	}
	fi, err := f.Stat()
	if err != nil {
		return proto.Attr{}, Errno(err)
	}
	return attrOf(fi), 0
}

func (s *Server) mkdir(p string, mode uint32) (proto.Attr, uint32) {
	if s.readOnly {
		return proto.Attr{}, proto.EROFS
	}
	if p == "" {
		return proto.Attr{}, proto.EEXIST
	}
	if err := s.root.Mkdir(p, fs.FileMode(mode&0o777)); err != nil {
		return proto.Attr{}, Errno(err)
	}
	f, err := s.root.Open(p)
	if err != nil {
		return proto.Attr{}, Errno(err)
	}
	defer f.Close()
	s.chown(f)
	_ = f.Chmod(fs.FileMode(mode & 0o777))
	fi, err := f.Stat()
	if err != nil {
		return proto.Attr{}, Errno(err)
	}
	return attrOf(fi), 0
}

func (s *Server) remove(p string, dir bool) uint32 {
	if s.readOnly {
		return proto.EROFS
	}
	if p == "" {
		return proto.EBUSY
	}
	fi, err := s.root.Lstat(p)
	if err != nil {
		return Errno(err)
	}
	switch {
	case dir && !fi.IsDir():
		return proto.ENOTDIR
	case !dir && fi.IsDir():
		return proto.EISDIR
	}
	// Remove de os.Root es unlinkat sobre el padre: no sigue el último
	// componente, así que borrar un enlace borra el enlace.
	if err := s.root.Remove(p); err != nil {
		return Errno(err)
	}
	return 0
}

// parent abre, dentro de la carpeta, el directorio padre de p y devuelve su
// descriptor y el último componente. Las operaciones *at sobre ese descriptor
// nunca siguen el último componente, y el descriptor no puede estar fuera.
func (s *Server) parent(p string) (*os.File, string, uint32) {
	dir, base := "", p
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			dir, base = p[:i], p[i+1:]
			break
		}
	}
	f, err := s.root.Open(rel(dir))
	if err != nil {
		return nil, "", Errno(err)
	}
	fi, err := f.Stat()
	if err != nil || !fi.IsDir() {
		f.Close()
		if err != nil {
			return nil, "", Errno(err)
		}
		return nil, "", proto.ENOTDIR
	}
	return f, base, 0
}

func (s *Server) rename(oldp, newp string, flags uint32) uint32 {
	if s.readOnly {
		return proto.EROFS
	}
	if oldp == "" || newp == "" {
		return proto.EBUSY
	}
	if flags&^proto.RenameNoReplace != 0 {
		return proto.EINVAL
	}
	of, ob, e := s.parent(oldp)
	if e != 0 {
		return e
	}
	defer of.Close()
	nf, nb, e := s.parent(newp)
	if e != 0 {
		return e
	}
	defer nf.Close()
	if flags&proto.RenameNoReplace != 0 {
		// Comprobar y luego renombrar: no es atómico frente a otro escritor del
		// host, y está documentado. Frente al invitado sí, porque sus
		// operaciones sobre este directorio las serializa su kernel.
		if _, err := s.root.Lstat(newp); err == nil {
			return proto.EEXIST
		} else if !errors.Is(err, fs.ErrNotExist) {
			return Errno(err)
		}
	}
	if err := renameat(of, ob, nf, nb); err != nil {
		return Errno(err)
	}
	return 0
}

func (s *Server) readlink(p string) (string, uint32) {
	if p == "" {
		return "", proto.EINVAL
	}
	f, base, e := s.parent(p)
	if e != 0 {
		return "", e
	}
	defer f.Close()
	t, err := readlinkat(f, base)
	if err != nil {
		return "", Errno(err)
	}
	return t, 0
}

func (s *Server) statfs() (proto.Statfs, uint32) {
	f, err := s.root.Open(".")
	if err != nil {
		return proto.Statfs{}, Errno(err)
	}
	defer f.Close()
	st, err := fstatfs(f)
	if err != nil {
		return proto.Statfs{}, Errno(err)
	}
	return st, 0
}

// ── atributos y errores ──────────────────────────────────────────────────────

func typeBits(m fs.FileMode) uint32 {
	switch m.Type() {
	case fs.ModeDir:
		return proto.SIFDIR
	case fs.ModeSymlink:
		return proto.SIFLNK
	}
	return proto.SIFREG
}

// attrOf traduce los atributos del host a los del invitado. El dueño no viaja:
// para el invitado todo es de root (uid/gid 0), y los bits setuid/setgid/sticky
// se pierden aquí.
func attrOf(fi fs.FileInfo) proto.Attr {
	a := proto.Attr{
		Mode:  typeBits(fi.Mode()) | uint32(fi.Mode().Perm()),
		Size:  uint64(fi.Size()),
		Nlink: 1,
	}
	mt := fi.ModTime()
	a.Mtime, a.Mtimen = mt.Unix(), uint32(mt.Nanosecond())
	a.Atime, a.Atimen = a.Mtime, a.Mtimen
	a.Ctime, a.Ctimen = a.Mtime, a.Mtimen
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		a.Blocks = uint64(st.Blocks)
		a.Nlink = uint32(st.Nlink)
		at, ct := statTimes(st)
		a.Atime, a.Atimen = at.Unix(), uint32(at.Nanosecond())
		a.Ctime, a.Ctimen = ct.Unix(), uint32(ct.Nanosecond())
	}
	if a.Blocks == 0 && a.Size > 0 {
		a.Blocks = (a.Size + 511) / 512
	}
	return a
}

func owner(fi fs.FileInfo) (int, int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

// Errno traduce un error del host al errno de Linux que verá el invitado. Por
// nombre y no por número: en macOS los números son otros.
func Errno(err error) uint32 {
	if err == nil {
		return 0
	}
	var en syscall.Errno
	if errors.As(err, &en) {
		switch en {
		case syscall.EPERM:
			return proto.EPERM
		case syscall.ENOENT:
			return proto.ENOENT
		case syscall.EBADF:
			return proto.EBADF
		case syscall.EAGAIN:
			return proto.EAGAIN
		case syscall.EACCES:
			return proto.EACCES
		case syscall.EBUSY, syscall.ETXTBSY:
			return proto.EBUSY
		case syscall.EEXIST:
			return proto.EEXIST
		case syscall.EXDEV:
			return proto.EXDEV
		case syscall.ENOTDIR:
			return proto.ENOTDIR
		case syscall.EISDIR:
			return proto.EISDIR
		case syscall.EINVAL:
			return proto.EINVAL
		case syscall.EMFILE, syscall.ENFILE:
			return proto.EMFILE
		case syscall.EFBIG:
			return proto.EFBIG
		case syscall.ENOSPC, syscall.EDQUOT:
			return proto.ENOSPC
		case syscall.EROFS:
			return proto.EROFS
		case syscall.ENAMETOOLONG:
			return proto.ENAMETOOLONG
		case syscall.ENOTEMPTY:
			return proto.ENOTEMPTY
		case syscall.ELOOP:
			return proto.ELOOP
		case syscall.ENOTSUP:
			return proto.EOPNOTSUPP
		case syscall.ERANGE:
			return proto.ERANGE
		}
		return proto.EIO
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return proto.ENOENT
	case errors.Is(err, fs.ErrExist):
		return proto.EEXIST
	case errors.Is(err, fs.ErrPermission):
		return proto.EACCES
	}
	// os.Root devuelve un error propio ("path escapes from parent") cuando una
	// ruta o un enlace saldrían de la carpeta. Para el invitado es un permiso
	// denegado: no hay nada ahí que él pueda ver.
	if strings.Contains(err.Error(), "escapes") {
		return proto.EACCES
	}
	return proto.EIO
}

// String para los mensajes de registro.
func (s *Server) String() string { return fmt.Sprintf("share %s (ro=%v)", s.dir, s.readOnly) }
