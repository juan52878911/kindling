package guest

// FUSE_INTERRUPT.
//
// Una petición ya leída por el agente no la cancela el kernel: el proceso que
// la hizo espera su respuesta aunque reciba SIGKILL. Si el daemon está colgado
// en una E/S del host (la conexión sigue viva, pero no contesta), ese proceso
// queda bloqueado hasta que la sesión se corte.
//
// Al llegar el INTERRUPT de una petición de solo lectura que sigue esperando,
// se contesta EINTR en el acto y la respuesta de verdad, cuando llegue, se
// tira. Las mutadoras no: con la operación quizá ya hecha en el host, EINTR
// mentiría. Tampoco las que crean un handle (OPEN, OPENDIR, CREATE): el
// kernel nunca sabría de él y nadie lo soltaría. La goroutine de la petición
// sigue ocupando su plaza hasta que el daemon conteste o la sesión se corte.

import "sync/atomic"

// errnoEINTR es EINTR de Linux.
const errnoEINTR uint32 = 4

// peticionViva es una petición interrumpible en vuelo. contestada la gana quien
// primero responde: la propia petición o el INTERRUPT.
type peticionViva struct {
	contestada atomic.Bool
}

// interrumpible dice qué peticiones se pueden contestar con EINTR sin que el
// resultado cambie nada: las de solo lectura. LOOKUP cuenta una referencia en
// la tabla que el kernel no sabrá soltar si recibe EINTR; solo retiene el nodo
// hasta que el tope lo expulse.
func interrumpible(op uint32) bool {
	switch op {
	case opLookup, opGetattr, opReadlink, opRead, opReaddir, opReaddirplus,
		opStatfs, opAccess:
		return true
	}
	return false
}

// apuntar registra req si es interrumpible; nil si no.
func (fs *fuseFS) apuntar(req fuseReq) *peticionViva {
	if !interrumpible(req.opcode) {
		return nil
	}
	p := &peticionViva{}
	fs.vmu.Lock()
	if fs.enVuelo == nil {
		fs.enVuelo = map[uint64]*peticionViva{}
	}
	fs.enVuelo[req.unique] = p
	fs.vmu.Unlock()
	return p
}

// despachar la quita del registro y dice si a la petición le toca contestar
// (false: ya se contestó EINTR).
func (fs *fuseFS) despachar(unique uint64, p *peticionViva) bool {
	fs.vmu.Lock()
	if fs.enVuelo[unique] == p {
		delete(fs.enVuelo, unique)
	}
	fs.vmu.Unlock()
	return p.contestada.CompareAndSwap(false, true)
}

// interrumpir atiende un FUSE_INTERRUPT (arg: el unique interrumpido).
func (fs *fuseFS) interrumpir(arg []byte) {
	if len(arg) < 8 {
		return
	}
	u := le.Uint64(arg)
	fs.vmu.Lock()
	p := fs.enVuelo[u]
	fs.vmu.Unlock()
	if p != nil && p.contestada.CompareAndSwap(false, true) {
		fs.write(reply(u, errnoEINTR))
	}
}
