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
// flush o fsync del MISMO handle, como en NFS: close(2) sigue devolviendo el
// error. Era del nodo, y con dos descriptores del mismo fichero el primero que
// cerraba se llevaba el error del otro, que cerraba limpio y perdía sus datos
// sin enterarse.
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

	share "github.com/juan52878911/kindling/internal/shareproto"
)

// wbMaxBytes acota lo pendiente (acumulado y en vuelo) por nodo.
const wbMaxBytes = 4 * share.MaxIO

// wbMaxFlying acota los trozos en vuelo por nodo, y con ellos las goroutines
// de wbFly. wbMaxBytes no basta: con escrituras pequeñas y no contiguas cada
// una es su propio trozo, y 4 MiB en trozos de 4 KiB eran 1024 a la vez. Más
// allá, como con los bytes, contrapresión.
const wbMaxFlying = 16

// wbGlobalMax acota lo pendiente de TODOS los nodos del proceso (en el agente,
// todas las carpetas; en kling-hv, las de su única VM). wbMaxBytes es por
// nodo: un `tar x` que deja muchos ficheros escribiéndose a la vez eran
// 4 MiB por fichero sin suma acotada, y en un invitado de 256 MiB eso es el
// OOM. Lleno, el write espera como con el tope por nodo.
const wbGlobalMax = 16 * share.MaxIO

// wbBudget son los bytes pendientes de todos los nodos.
type wbBudget struct {
	mu   sync.Mutex
	cond sync.Cond
	used int
	max  int
}

var wbGlobal = newWBBudget(wbGlobalMax)

func newWBBudget(max int) *wbBudget {
	b := &wbBudget{max: max}
	b.cond.L = &b.mu
	return b
}

// tryTake reserva n si caben. Con nada reservado caben siempre: un write
// solo nunca se queda esperando a sí mismo.
func (b *wbBudget) tryTake(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used > 0 && b.used+n > b.max {
		return false
	}
	b.used += n
	return true
}

// take reserva n, esperando a que quepan.
func (b *wbBudget) take(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.used > 0 && b.used+n > b.max {
		b.cond.Wait()
	}
	b.used += n
}

// force reserva n aunque no quepan: lo pendiente que trae un estado
// restaurado ya está en memoria.
func (b *wbBudget) force(n int) {
	b.mu.Lock()
	b.used += n
	b.mu.Unlock()
}

// give devuelve n.
func (b *wbBudget) give(n int) {
	if n == 0 {
		return
	}
	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
	b.cond.Broadcast()
}

func (b *wbBudget) enUso() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

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
	// errs es el primer error de un trozo ya contestado al kernel, por el
	// handle que lo escribió; sinDueño, uno que vino sin handle (el estado
	// restaurado de kling-hv guarda uno por nodo): se lo lleva el primero que
	// pregunte, como antes. err es uno de los dos, el que guarda el estado
	// (fuse_host.go); no se toca a mano: espejo.
	errs     map[*fileHandle]uint32
	sinDueño uint32
	err      uint32
}

// adoptar toma como sin dueño un err puesto desde fuera (al restaurar).
func (st *wbState) adoptar() {
	if len(st.errs) == 0 && st.sinDueño == 0 && st.err != 0 {
		st.sinDueño = st.err
	}
}

// espejo deja en err uno de los errores pendientes (o ninguno).
func (st *wbState) espejo() {
	st.err = st.sinDueño
	for _, e := range st.errs {
		st.err = e
		break
	}
}

// apuntarErr guarda el error de un trozo de h, si h no tenía ya uno.
func (st *wbState) apuntarErr(h *fileHandle, errno uint32) {
	st.adoptar()
	if st.errs == nil {
		st.errs = map[*fileHandle]uint32{}
	}
	if st.errs[h] == 0 {
		st.errs[h] = errno
	}
	st.espejo()
}

// tomarErr es el error pendiente que le toca a h: el suyo o, si no tiene, uno
// sin dueño. El de otro handle, nunca. Con take, lo consume.
func (st *wbState) tomarErr(h *fileHandle, take bool) uint32 {
	st.adoptar()
	if e := st.errs[h]; e != 0 {
		if take {
			delete(st.errs, h)
			st.espejo()
		}
		return e
	}
	e := st.sinDueño
	if take && e != 0 {
		st.sinDueño = 0
		st.espejo()
	}
	return e
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
	// El tope global, antes del cerrojo del nodo y sin tenerlo: lo reservado
	// lo devuelve wbFly, que necesita ese cerrojo, y esperar con él cogido
	// podría no acabar nunca.
	if !wbGlobal.tryTake(len(data)) {
		if !wait {
			return 0, false
		}
		wbGlobal.take(len(data))
	}
	encolado := false
	defer func() {
		if !encolado {
			wbGlobal.give(len(data))
		}
	}()
	st := wbOf(h.node)
	st.mu.Lock()
	defer st.mu.Unlock()
	if e := st.tomarErr(h, true); e != 0 {
		return e, true
	}
	// Cada espera suelta el cerrojo y otro puede haber cambiado st.cur: se
	// vuelve a mirar todo hasta que quepa.
	for {
		c := st.cur
		if c != nil && (c.h != h || c.end() != off || len(c.data)+len(data) > share.MaxIO) {
			if !wait && (st.clashLocked(c) || len(st.flying) >= wbMaxFlying) {
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
	encolado = true
	// Sale ya si no vuela nada (group commit) o si está lleno; uno lleno que
	// se solapa con algo en vuelo espera a que wbFly lo mande.
	if len(st.flying) == 0 || (len(st.cur.data) >= share.MaxIO && len(st.flying) < wbMaxFlying && !st.clashLocked(st.cur)) {
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
// que eso conteste: el orden de dos escrituras al mismo rango importa. Con
// wbMaxFlying trozos en vuelo, también espera a que vuelva uno.
func (fs *fuseFS) wbSendLocked(st *wbState) {
	c := st.cur
	if c == nil {
		return
	}
	for st.clashLocked(c) || len(st.flying) >= wbMaxFlying {
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
	wbGlobal.give(len(c.data))
	st.mu.Lock()
	defer st.mu.Unlock()
	for i, f := range st.flying {
		if f == c {
			st.flying = append(st.flying[:i], st.flying[i+1:]...)
			break
		}
	}
	st.bytes -= len(c.data)
	if errno != 0 {
		st.apuntarErr(c.h, errno)
	}
	// Lo que se acumuló mientras volaba este sale ya (group commit); y uno
	// lleno que esperaba plaza, también.
	if c := st.cur; c != nil && (len(st.flying) == 0 || (len(c.data) >= share.MaxIO && !st.clashLocked(c))) {
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
// esperando para siempre). Devuelve el error pendiente sin dueño, si lo hay, y
// take dice si se consume: el de un handle lo recibe ese handle (drainHandle).
func (fs *fuseFS) drain(n *node, take bool) uint32 { return fs.drainDe(n, nil, take) }

// drainHandle es drain del nodo de h, devolviendo el error pendiente de h
// (flush y fsync lo devuelven; leer o pedir atributos no son quien para
// recibir el error de una escritura).
func (fs *fuseFS) drainHandle(h *fileHandle, take bool) uint32 {
	if h == nil {
		return 0
	}
	return fs.drainDe(h.node, h, take)
}

func (fs *fuseFS) drainDe(n *node, h *fileHandle, take bool) uint32 {
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
	return st.tomarErr(h, take)
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
