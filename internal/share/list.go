package share

// Listado de directorios por páginas, con el directorio abierto entre una
// página y la siguiente.
//
// El protocolo pagina con una cookie que es el índice de la siguiente entrada
// (OpList: path, cookie, max). Antes cada página reabría el directorio y se
// saltaba `cookie` entradas: listar uno de n entradas costaba O(n²) lecturas
// de getdents, y el agente lo lista entero en cada opendir (un node_modules con
// miles de entradas, en cada `ls` o en cada require que recorra el árbol).
//
// Ahora, si una página no llega al final, el directorio abierto se guarda en
// la sesión junto a la cookie con la que sigue; la petición siguiente con esa
// ruta y esa cookie continúa donde iba. Si no hay cursor (caducó, se expulsó,
// o la cookie no coincide), se hace lo de antes: abrir y saltar. No cambia
// nada del protocolo, así que vale igual con agentes anteriores.

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"time"

	proto "github.com/juan52878911/kindling/pkg/share"
)

const (
	// maxListCursors por sesión: listados a medias guardados a la vez. Un
	// opendir del agente pide sus páginas seguidas, así que casi nunca hay más
	// de uno o dos.
	maxListCursors = 4
	// listCursorTTL: un cursor sin usar más que esto se cierra. Las páginas de
	// un mismo listado llegan con milisegundos de diferencia.
	listCursorTTL = 30 * time.Second
)

// listCursor es un listado a medias: el directorio abierto, lo ya leído de él
// que aún no se ha enviado, y la cookie con la que sigue (idx).
type listCursor struct {
	f    *os.File
	path string
	idx  uint64
	pend []fs.DirEntry
	eof  bool // ReadDir ya llegó al final (pend puede tener aún entradas)
	used time.Time
}

// list sirve OpList.
func (ss *session) list(id uint64, p string, cookie uint64, max int) []byte {
	c := ss.takeCursor(p, cookie)
	if c == nil {
		f, err := ss.srv.root.Open(rel(p))
		if err != nil {
			return errReply(id, Errno(err))
		}
		if fi, err := f.Stat(); err != nil {
			f.Close()
			return errReply(id, Errno(err))
		} else if !fi.IsDir() {
			f.Close()
			return errReply(id, proto.ENOTDIR)
		}
		c = &listCursor{f: f, path: p}
	}
	resp, more := c.page(id, cookie, max)
	if more {
		ss.keepCursor(c)
	} else {
		c.f.Close()
	}
	return resp
}

// page escribe la página que empieza en cookie. more dice si el listado sigue
// y el cursor vale para la siguiente.
func (c *listCursor) page(id, cookie uint64, max int) (resp []byte, more bool) {
	var ent proto.Enc
	n, eof := uint32(0), false
	for {
		if len(c.pend) == 0 {
			if c.eof {
				eof = true
				break
			}
			batch, err := c.f.ReadDir(256)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					return errReply(id, Errno(err)), false
				}
				c.eof = true
			} else if len(batch) == 0 {
				c.eof = true
			}
			c.pend = batch
			continue
		}
		de := c.pend[0]
		// Sin cursor se empieza por el principio y se salta hasta la cookie,
		// contando también lo que no se enseña: la cookie es la posición en
		// el directorio, no en lo enviado.
		if c.idx >= cookie && servible(de.Type()) && proto.ValidName(de.Name()) == nil {
			if len(ent.B)+len(de.Name())+6 > max {
				break
			}
			ent.Str(de.Name())
			ent.U32(typeBits(de.Type()))
			n++
		}
		c.idx++
		c.pend = c.pend[1:]
	}
	r := proto.Response(id, 0)
	if eof {
		r.U8(1)
	} else {
		r.U8(0)
	}
	r.U32(n)
	r.B = append(r.B, ent.B...)
	r.U64(c.idx)
	return r.B, !eof
}

// takeCursor saca de la sesión el cursor que sigue el listado de p en cookie,
// si lo hay, y cierra los caducados.
func (ss *session) takeCursor(p string, cookie uint64) *listCursor {
	now := time.Now()
	ss.mu.Lock()
	defer ss.mu.Unlock()
	var found *listCursor
	keep := ss.cursors[:0]
	for _, c := range ss.cursors {
		switch {
		case now.Sub(c.used) > listCursorTTL:
			ss.dropCursorLocked(c)
		case found == nil && c.path == p && c.idx == cookie:
			found = c
		default:
			keep = append(keep, c)
		}
	}
	clear(ss.cursors[len(keep):])
	ss.cursors = keep
	if found != nil {
		// Fuera de la sesión ya no ocupa presupuesto: lo vuelve a pedir
		// keepCursor si hay que guardarlo otra vez.
		<-ss.srv.budget.slots
		<-globalFDs
	}
	return found
}

// keepCursor guarda c para la página siguiente. Ocupa un descriptor del
// presupuesto de la máquina como cualquier handle; si no queda, no se guarda
// (la página siguiente reabre, como antes). Si ya hay maxListCursors, sale el
// más viejo.
func (ss *session) keepCursor(c *listCursor) {
	c.used = time.Now()
	ss.mu.Lock()
	defer ss.mu.Unlock()
	select {
	case ss.srv.budget.slots <- struct{}{}:
	default:
		c.f.Close()
		return
	}
	select {
	case globalFDs <- struct{}{}:
	default:
		<-ss.srv.budget.slots
		c.f.Close()
		return
	}
	if len(ss.cursors) >= maxListCursors {
		ss.dropCursorLocked(ss.cursors[0])
		ss.cursors = append(ss.cursors[:0], ss.cursors[1:]...)
	}
	ss.cursors = append(ss.cursors, c)
}

// dropCursorLocked cierra un cursor guardado y devuelve su descriptor al
// presupuesto. Con ss.mu tomado; quien llama lo saca de ss.cursors.
func (ss *session) dropCursorLocked(c *listCursor) {
	_ = c.f.Close()
	<-ss.srv.budget.slots
	<-globalFDs
}
