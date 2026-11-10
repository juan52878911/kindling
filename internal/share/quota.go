package share

// Cuota de una carpeta rw (M5): sin ella el invitado llena el disco del host
// escribiendo, alargando ficheros con SetSize o creando ficheros sin fin. Se
// cuenta el crecimiento NETO desde que la máquina empezó a usar la carpeta (el
// daemon guarda la cuenta al congelar, parar o apagarse: ver SetUsage): lo que
// alarga un fichero (WriteAt más allá del final, SetSize hacia arriba) suma;
// lo que lo encoge (SetSize hacia abajo, O_TRUNC) o lo borra (unlink del
// último nombre, ya cerrado) resta, sin bajar nunca de cero: borrar lo que ya estaba en la carpeta no da presupuesto para
// escribir más. Ver docs/compartir.md, «Cuota de las carpetas rw».
//
// Se cuenta el tamaño aparente, no los bloques: un fichero disperso cuenta
// entero. Escribir en un hueco de un fichero que ya existía no lo alarga y no
// se cuenta; para eso (y para lo que escriba el propio host) está el mínimo
// libre del sistema de ficheros, que se mira en lo que alarga, en lo que crea
// entradas y en lo que escribe dentro de un fichero disperso. Reescribir en su
// sitio un fichero sin huecos no gasta disco y no se para por el mínimo: con
// el disco del host casi lleno, una base de datos que reescribe sus páginas
// seguía recibiendo ENOSPC sin haber pedido ni un bloque más.

import (
	"io/fs"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	proto "github.com/juan52878911/kindling/internal/shareproto"
)

// Limits son los topes de una carpeta rw. 0 en un campo = sin ese tope.
type Limits struct {
	// MaxBytes es el crecimiento neto en bytes que puede sumar la carpeta.
	MaxBytes int64
	// MaxFiles son las entradas nuevas (ficheros, directorios, enlaces) netas.
	MaxFiles int64
	// MinFree son los bytes libres que tienen que quedar en el sistema de
	// ficheros del host: por debajo, lo que escribe o crea da ENOSPC.
	MinFree int64
}

// statfsCache es cuánto vale una lectura de statfs del host. Entre lecturas se
// le descuenta lo que se va reservando, así que lo que se cuela en ese rato es
// como mucho lo que escribe el host por su lado.
const statfsCache = 250 * time.Millisecond

// Quota lleva la cuenta de una carpeta. Es de la carpeta, no de la conexión:
// el daemon la comparte entre las sesiones (y los Server) de las reconexiones,
// así que cortar y volver a conectar no la pone a cero.
type Quota struct {
	lim Limits

	// size ordena los cambios de tamaño: una escritura que no alarga el
	// fichero va con RLock (en paralelo con las demás), y las que alargan, los
	// SetSize y los O_TRUNC, con Lock. Sin él, entre el Stat y el WriteAt otro
	// podía encoger el fichero (y cobrar la devolución) y la escritura lo
	// alargaba sin contar.
	size  sync.RWMutex
	bytes atomic.Int64
	files atomic.Int64

	// abiertos cuenta los handles vivos de cada fichero: un unlink de algo
	// abierto no libera el disco hasta el último cierre, y es entonces cuando
	// se devuelve. Con amu, que también ordena unlink frente a registrar.
	amu      sync.Mutex
	abiertos map[Ident]int

	fmu    sync.Mutex
	freeAt time.Time
	free   int64
	// bajo: la última vez se negó algo por el mínimo libre. Para avisar en el
	// log al cruzarlo (en los dos sentidos) y no en cada escritura.
	bajo bool
}

// NewQuota crea la cuenta de una carpeta con los topes l.
func NewQuota(l Limits) *Quota { return &Quota{lim: l, abiertos: map[Ident]int{}} }

// SetUsage pone la cuenta en bytes y files, lo que llevaba la carpeta en un
// arranque anterior (el daemon lo guarda al congelar o parar). Antes de servir.
func (q *Quota) SetUsage(bytes, files int64) {
	q.bytes.Store(max(bytes, 0))
	q.files.Store(max(files, 0))
}

// Usage devuelve los bytes y las entradas netas que lleva la carpeta.
func (q *Quota) Usage() (bytes, files int64) { return q.bytes.Load(), q.files.Load() }

// SetQuota pone la cuenta de la carpeta. Antes de servir; nil = sin cuota.
// Una carpeta de solo lectura la ignora.
func (s *Server) SetQuota(q *Quota) {
	if !s.readOnly {
		s.quota = q
	}
}

// libre dice si quedan n bytes por encima del mínimo libre del host, y los
// descuenta de la lectura guardada.
func (q *Quota) libre(s *Server, n int64) bool {
	if q.lim.MinFree <= 0 {
		return true
	}
	q.fmu.Lock()
	defer q.fmu.Unlock()
	if now := time.Now(); now.Sub(q.freeAt) > statfsCache {
		st, e := s.statfs()
		if e != 0 {
			// Sin statfs no se sabe: no se bloquea al invitado por eso.
			return true
		}
		q.free, q.freeAt = int64(st.Bavail)*int64(st.Bsize), now
	}
	if q.free-n < q.lim.MinFree {
		if !q.bajo {
			q.bajo = true
			log.Printf("share %s: the host file system is below its free minimum (%d MiB free, daemon.share_min_free_mib = %d): writes that need disk get ENOSPC",
				s.dir, q.free>>20, q.lim.MinFree>>20)
		}
		return false
	}
	if q.bajo {
		q.bajo = false
		log.Printf("share %s: the host file system has room again (%d MiB free)", s.dir, q.free>>20)
	}
	q.free -= n
	return true
}

// lleno dice si fi no tiene huecos: sus bloques cubren su tamaño. Reescribir
// dentro de un fichero así no pide disco nuevo. Sin el dato (otro sistema),
// no: se mira el mínimo libre como siempre.
func lleno(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int64(st.Blocks)*512 >= fi.Size()
}

// reservar cobra n bytes de crecimiento; ENOSPC si no caben. Con q.size en
// exclusiva.
func (q *Quota) reservar(s *Server, n int64) uint32 {
	if n <= 0 {
		return 0
	}
	if q.lim.MaxBytes > 0 && q.bytes.Load()+n > q.lim.MaxBytes {
		return proto.ENOSPC
	}
	if !q.libre(s, n) {
		return proto.ENOSPC
	}
	q.bytes.Add(n)
	return 0
}

// devolver resta n de c sin bajar de cero.
func devolver(c *atomic.Int64, n int64) {
	if n <= 0 {
		return
	}
	for {
		v := c.Load()
		nv := max(v-n, 0)
		if c.CompareAndSwap(v, nv) {
			return
		}
	}
}

// reservarEntrada cobra una entrada nueva; ENOSPC si no cabe.
func (q *Quota) reservarEntrada(s *Server) uint32 {
	if q.lim.MaxFiles > 0 && q.files.Add(1) > q.lim.MaxFiles {
		devolver(&q.files, 1)
		return proto.ENOSPC
	}
	if q.lim.MaxFiles <= 0 {
		q.files.Add(1)
	}
	if !q.libre(s, 0) {
		devolver(&q.files, 1)
		return proto.ENOSPC
	}
	return 0
}

// writeAt escribe data en off contando lo que alarga f.
func (s *Server) writeAt(f *os.File, data []byte, off int64) (int, uint32) {
	q := s.quota
	if q == nil {
		n, err := f.WriteAt(data, off)
		if err != nil && n == 0 {
			return 0, Errno(err)
		}
		return n, 0
	}
	end := off + int64(len(data))
	q.size.RLock()
	fi, err := f.Stat()
	if err == nil && end <= fi.Size() {
		// No alarga: solo el mínimo libre, y solo si puede rellenar un hueco.
		if !lleno(fi) && !q.libre(s, 0) {
			q.size.RUnlock()
			return 0, proto.ENOSPC
		}
		n, err := f.WriteAt(data, off)
		q.size.RUnlock()
		if err != nil && n == 0 {
			return 0, Errno(err)
		}
		return n, 0
	}
	q.size.RUnlock()

	q.size.Lock()
	defer q.size.Unlock()
	fi, err = f.Stat()
	if err != nil {
		return 0, Errno(err)
	}
	antes := fi.Size()
	crece := max(end-antes, 0)
	if e := q.reservar(s, crece); e != 0 {
		return 0, e
	}
	n, err := f.WriteAt(data, off)
	// Lo reservado que no se llegó a escribir vuelve.
	devolver(&q.bytes, crece-max(off+int64(n)-antes, 0))
	if err != nil && n == 0 {
		return 0, Errno(err)
	}
	return n, 0
}

// truncate cambia el tamaño de f contando la diferencia.
func (s *Server) truncate(f *os.File, size int64) uint32 {
	q := s.quota
	if q == nil {
		if err := f.Truncate(size); err != nil {
			return Errno(err)
		}
		return 0
	}
	q.size.Lock()
	defer q.size.Unlock()
	fi, err := f.Stat()
	if err != nil {
		return Errno(err)
	}
	antes := fi.Size()
	if e := q.reservar(s, size-antes); e != 0 {
		return e
	}
	if err := f.Truncate(size); err != nil {
		devolver(&q.bytes, size-antes)
		return Errno(err)
	}
	devolver(&q.bytes, antes-size)
	return 0
}

// nlink es el número de nombres de fi; 0 si el sistema no lo da.
func nlink(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 0
}

// abrir apunta un handle vivo de f. Un fichero que ya no tiene nombre (lo
// borraron entre abrirlo y apuntarlo, y ese unlink ya devolvió su tamaño) no
// se apunta: ENOENT, como si el open hubiera llegado después del unlink.
func (q *Quota) abrir(f *os.File) uint32 {
	fi, err := f.Stat()
	if err != nil {
		return Errno(err)
	}
	id, ok := IdentOf(fi)
	if !ok {
		return 0
	}
	q.amu.Lock()
	defer q.amu.Unlock()
	// Con amu: un unlink no puede colarse entre esta lectura y el apunte.
	if fi, err = f.Stat(); err != nil {
		return Errno(err)
	}
	if nlink(fi) == 0 {
		return proto.ENOENT
	}
	q.abiertos[id]++
	return 0
}

// cerrar suelta el handle de f (antes de cerrarlo). Si era el último y el
// fichero ya no tiene nombre, su disco se libera ahora: se devuelve.
func (q *Quota) cerrar(f *os.File) {
	fi, err := f.Stat()
	if err != nil {
		return
	}
	id, ok := IdentOf(fi)
	if !ok {
		return
	}
	q.amu.Lock()
	defer q.amu.Unlock()
	if q.abiertos[id] <= 1 {
		delete(q.abiertos, id)
		if nlink(fi) == 0 {
			devolver(&q.bytes, fi.Size())
			devolver(&q.files, 1)
		}
		return
	}
	q.abiertos[id]--
}

// borrar hace lo de remove (lstat, comprobar, rm) con los tamaños quietos y
// devuelve lo que libera: la entrada si era el último nombre, y su tamaño si
// además nadie lo tiene abierto (si no, al último cierre). Con q.size en
// exclusiva: un Lstat suelto podía ver el tamaño de antes de un SetSize que
// encoge y devolver dos veces lo mismo.
func (q *Quota) borrar(lstat func() (fs.FileInfo, error), check func(fs.FileInfo) uint32, rm func() error) uint32 {
	q.size.Lock()
	defer q.size.Unlock()
	q.amu.Lock()
	defer q.amu.Unlock()
	fi, err := lstat()
	if err != nil {
		return Errno(err)
	}
	if e := check(fi); e != 0 {
		return e
	}
	if err := rm(); err != nil {
		return Errno(err)
	}
	switch {
	case fi.IsDir():
		devolver(&q.files, 1)
	case nlink(fi) > 1:
	case !fi.Mode().IsRegular():
		devolver(&q.files, 1)
	default:
		if id, ok := IdentOf(fi); ok && q.abiertos[id] > 0 {
			return 0
		}
		devolver(&q.bytes, fi.Size())
		devolver(&q.files, 1)
	}
	return 0
}
