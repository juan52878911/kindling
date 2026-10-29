package guest

// Escritura diferida en el agente (write-behind).
//
// Sin ella cada write(2) del invitado es un viaje de ida y vuelta al daemon
// antes de volver: 4 KiB a 0,3 ms son ~3 000 escrituras por segundo, y un
// fichero de 1 MiB se escribe a la velocidad de un viaje por trozo. Con ella
// el agente contesta al kernel en cuanto tiene los datos, los junta con los
// contiguos del mismo handle y los manda en trozos de hasta share.MaxIO:
//
//   - en cuanto no hay nada en vuelo del nodo, sale lo acumulado (como el
//     «group commit» de una base de datos): la latencia hasta el host es la de
//     un viaje, y mientras vuela uno se acumula el siguiente;
//   - un trozo lleno (share.MaxIO) sale aunque haya otros en vuelo, hasta
//     wbMaxBytes por nodo; más allá, el write espera (contrapresión);
//   - dos trozos que se solapan nunca vuelan a la vez: el daemon los
//     ejecutaría en cualquier orden.
//
// Lo que el kernel pida después de un nodo con datos pendientes —leer,
// atributos, truncar, abrir, fsync, flush, release, buscarlo por nombre— espera
// antes a que lleguen (drain): nadie ve el fichero sin lo que ya se escribió.
// Un error de un trozo ya contestado se guarda y lo recibe el siguiente write,
// flush o fsync de ese nodo, como en NFS: close(2) sigue devolviendo el error.
//
// Por qué no FUSE_WRITEBACK_CACHE del kernel, que haría lo mismo con la caché
// de páginas: con él, Linux da por buenos su tamaño y su mtime de cada fichero
// regular y deja de creerse los del host (fuse_change_attributes, cache_mask).
// Un fichero que el host alarga o acorta se seguiría viendo con el tamaño viejo
// hasta que el inodo saliera de memoria, y ni AUTO_INVAL_DATA ni los avisos de
// WATCH lo arreglan (INVAL_INODE no toca i_size). Aquí el kernel no cambia de
// modo: atributos y coherencia con el host son los de siempre.
//
// Solo con un daemon que lo anuncia en OpHello (share.CapWriteBehind); con uno
// anterior, cada write espera su respuesta, como antes.

import (
	"sync"

	"github.com/juan52878911/kindling/pkg/share"
)

// wbMaxBytes acota lo pendiente (acumulado y en vuelo) por nodo.
const wbMaxBytes = 4 * share.MaxIO

// wbChunk es un trozo contiguo de un handle: acumulándose o en vuelo.
type wbChunk struct {
	h    *fileHandle
	off  uint64
	data []byte
}

func (c *wbChunk) end() uint64 { return c.off + uint64(len(c.data)) }

func (c *wbChunk) overlaps(off, end uint64) bool { return c.off < end && off < c.end() }

// wbState es lo pendiente de un nodo. Con mu.
type wbState struct {
	mu     sync.Mutex
	cond   sync.Cond
	cur    *wbChunk   // acumulándose, aún sin mandar
	flying []*wbChunk // mandados, sin respuesta
	bytes  int        // cur + flying
	err    uint32     // primer error de un trozo ya contestado al kernel
}

// wbOf devuelve el estado de escritura diferida de n, creándolo.
func wbOf(n *node) *wbState {
	if st := n.wb.Load(); st != nil {
		return st
	}
	st := &wbState{}
	st.cond.L = &st.mu
	if n.wb.CompareAndSwap(nil, st) {
		return st
	}
	return n.wb.Load()
}

// writeBehind encola una escritura y contesta en seguida. Devuelve el error
// pendiente de un trozo anterior, si lo hay (y entonces no encola nada). Con
// wait false no espera nunca: si habría que esperar (contrapresión, o un
// trozo que se solapa con otro en vuelo) devuelve ok false sin tocar nada, y
// el llamador lo repite en su goroutine con wait true.
func (fs *fuseFS) writeBehind(h *fileHandle, off uint64, data []byte, wait bool) (errno uint32, ok bool) {
	st := wbOf(h.node)
	st.mu.Lock()
	defer st.mu.Unlock()
	if e := st.err; e != 0 {
		st.err = 0
		return e, true
	}
	// Cada espera suelta el cerrojo y otro puede haber cambiado st.cur: se
	// vuelve a mirar todo hasta que quepa.
	for {
		c := st.cur
		if c != nil && (c.h != h || c.end() != off || len(c.data)+len(data) > share.MaxIO) {
			if !wait && st.clashLocked(c) {
				return 0, false
			}
			fs.wbSendLocked(st)
			continue
		}
		// Contrapresión: lo pendiente del nodo no pasa de wbMaxBytes.
		if st.bytes > 0 && st.bytes+len(data) > wbMaxBytes {
			if !wait {
				return 0, false
			}
			if c != nil && len(st.flying) == 0 {
				fs.wbSendLocked(st)
			} else {
				st.cond.Wait()
			}
			continue
		}
		break
	}
	if st.cur == nil {
		st.cur = &wbChunk{h: h, off: off, data: make([]byte, 0, max(len(data), 64<<10))}
	}
	st.cur.data = append(st.cur.data, data...)
	st.bytes += len(data)
	// Sale ya si no vuela nada (group commit) o si está lleno; uno lleno que
	// se solapa con algo en vuelo espera a que wbFly lo mande.
	if len(st.flying) == 0 || (len(st.cur.data) >= share.MaxIO && !st.clashLocked(st.cur)) {
		fs.wbSendLocked(st)
	}
	return 0, true
}

// clashLocked dice si c se solapa con algo en vuelo.
func (st *wbState) clashLocked(c *wbChunk) bool {
	for _, f := range st.flying {
		if f.overlaps(c.off, c.end()) {
			return true
		}
	}
	return false
}

// wbSendLocked manda st.cur. Si se solapa con algo en vuelo, espera antes a
// que eso conteste: el orden de dos escrituras al mismo rango importa.
func (fs *fuseFS) wbSendLocked(st *wbState) {
	c := st.cur
	if c == nil {
		return
	}
	for st.clashLocked(c) {
		st.cond.Wait()
		if st.cur != c {
			return // lo mandó otro mientras se esperaba
		}
	}
	st.cur = nil
	st.flying = append(st.flying, c)
	go fs.wbFly(st, c)
}

// wbFly escribe c en el host y lo retira de lo pendiente.
func (fs *fuseFS) wbFly(st *wbState, c *wbChunk) {
	errno := fs.writeAll(c.h, c.off, c.data)
	st.mu.Lock()
	defer st.mu.Unlock()
	for i, f := range st.flying {
		if f == c {
			st.flying = append(st.flying[:i], st.flying[i+1:]...)
			break
		}
	}
	st.bytes -= len(c.data)
	if errno != 0 && st.err == 0 {
		st.err = errno
	}
	// Lo que se acumuló mientras volaba este sale ya (group commit).
	if len(st.flying) == 0 && st.cur != nil {
		fs.wbSendLocked(st)
	}
	st.cond.Broadcast()
}

// writeAll escribe data en off por el handle h, troceada al tamaño de la
// sesión. Devuelve cuánto se escribió y el error, si no se escribió todo.
func (fs *fuseFS) writeAll(h *fileHandle, off uint64, data []byte) uint32 {
	done, errno := fs.writeSync(h, off, data)
	if errno == 0 && done < uint32(len(data)) {
		// El host escribió menos sin decir por qué: ya no se puede devolver
		// como escritura corta, el kernel la dio por hecha.
		errno = share.EIO
	}
	return errno
}

// writeSync es la escritura de siempre: troceada al tamaño de la sesión, y
// con trozos ya escritos un fallo da una escritura corta y no un error (el
// kernel y write(2) saben qué hacer con ella, y lo escrito, escrito está).
func (fs *fuseFS) writeSync(h *fileHandle, off uint64, data []byte) (uint32, uint32) {
	var done uint32
	size := uint32(len(data))
	for done < size {
		pos, rest := off+uint64(done), data[done:]
		var asked uint32
		errno, d := fs.handleCallRaw(h, func(s *shareSession, dh uint64) (uint32, *share.Dec) {
			asked = min(uint32(len(rest)), s.maxIO)
			return s.call(share.OpWrite, func(e *share.Enc) { e.U64(dh); e.U64(pos); e.Bytes(rest[:asked]) })
		})
		if errno != 0 {
			if done > 0 {
				break
			}
			return 0, errno
		}
		w := d.U32()
		if d.Err() != nil || w > asked {
			return done, share.EIO
		}
		done += w
		if w < asked {
			break
		}
	}
	return done, 0
}

// drain espera a que llegue al host lo que estaba pendiente de n al llamarla
// (lo que se escriba mientras espera ya no: un escritor continuo no la deja
// esperando para siempre). take dice si se consume el error pendiente (flush y
// fsync lo devuelven; leer o pedir atributos no son quien para recibir el
// error de una escritura).
func (fs *fuseFS) drain(n *node, take bool) uint32 {
	if n == nil {
		return 0
	}
	st := n.wb.Load()
	if st == nil {
		return 0
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	wait := append([]*wbChunk(nil), st.flying...)
	if st.cur != nil {
		wait = append(wait, st.cur)
	}
	for _, c := range wait {
		for st.pending(c) {
			if st.cur == c && len(st.flying) == 0 {
				fs.wbSendLocked(st)
				continue
			}
			st.cond.Wait()
		}
	}
	e := st.err
	if take {
		st.err = 0
	}
	return e
}

// pending dice si c aún no ha llegado al host.
func (st *wbState) pending(c *wbChunk) bool {
	if st.cur == c {
		return true
	}
	for _, f := range st.flying {
		if f == c {
			return true
		}
	}
	return false
}

// wbEnabled dice si las escrituras pueden diferirse con la sesión actual.
func (fs *fuseFS) wbEnabled() bool {
	if fs.readOnly {
		return false
	}
	fs.smu.Lock()
	s := fs.sess
	fs.smu.Unlock()
	return s != nil && s.caps&share.CapWriteBehind != 0
}
