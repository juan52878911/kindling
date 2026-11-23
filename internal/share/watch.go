package share

// Avisos de cambios hechos en el host (OpWatch).
//
// Sin ellos, el invitado ve un cambio del host cuando caducan sus cachés (1 s)
// y ningún vigilante del invitado (inotify: `node --watch`, nodemon, vite,
// webpack...) se entera: inotify solo ve lo que pasa por el VFS del propio
// invitado. Con OpWatch el daemon vigila en el host lo que el invitado ya ha
// mirado y le manda un aviso por cada cambio; el agente invalida la caché del
// kernel y hace que inotify dispare (ver pkg/guest/fuse_notify.go).
//
// Qué se vigila: solo las rutas que el invitado ha nombrado (stat, list, open,
// create, mkdir), con tope por sesión y expulsión de la menos usada. Lo que el
// invitado nunca miró no puede estar en su caché ni tener un vigilante encima,
// así que no hace falta recorrer el árbol (un node_modules de 50 000 ficheros
// no cuesta nada mientras nadie lo mire). En macOS, kqueue con un descriptor
// O_EVTONLY por ruta; en Linux, inotify, sin descriptores.
//
// Los cambios que hace el propio invitado también llegan al vigilante del
// host: se descartan los de rutas que el invitado tocó hace menos de
// guestEcho, o sus herramientas verían cada cambio dos veces.
//
// Si el sistema pierde eventos (la cola de inotify se desborda: IN_Q_OVERFLOW)
// no se sabe qué cambió, así que se repasa todo lo vigilado (resync): cada
// directorio contra su foto y cada fichero contra su huella (tamaño, mtime,
// inodo), y se avisa de lo que difiera. Si no se puede vigilar una ruta más
// (el tope de descriptores de kqueue) se dice una vez en el log: esa ruta se
// ve al caducar la caché de 1 s, pero sus vigilantes del invitado no disparan.

import (
	"container/list"
	"errors"
	"io"
	"io/fs"
	"log"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	proto "github.com/juan52878911/kindling/internal/shareproto"
)

var (
	// maxWatchesPerSession acota las rutas vigiladas por conexión (pasado el
	// tope se suelta la menos reciente).
	maxWatchesPerSession = watchesPorSesion
	// maxKernelWatches acota las que van con una vigilancia del sistema
	// (kqueue, inotify); pasado el tope, la menos reciente de ellas pasa a
	// vigilarse por sondeo.
	maxKernelWatches = kernelPorSesion
	// maxDirSnapshot: un directorio con más entradas no se compara entrada a
	// entrada (se avisa del directorio, no de cada hijo).
	maxDirSnapshot = 20000
	// maxSnapBytes acota lo que ocupan, sumadas, las fotos de directorio de
	// una sesión (estimado con snapCost: unos 270 000 nombres cortos). Sin él,
	// 4 096 rutas vigiladas de 20 000 entradas cada una eran gigas de heap del
	// daemon. Pasado el tope un directorio se vigila sin foto: de sus cambios
	// se avisa del directorio entero, no de cada hijo.
	maxSnapBytes int64 = 16 << 20
	// watchDebounce agrupa ráfagas (un editor que guarda escribe varias veces).
	watchDebounce = 15 * time.Millisecond
	// guestEcho: cuánto tiempo se considera eco un cambio en una ruta que tocó
	// el invitado.
	guestEcho = time.Second
	// maxEcho acota las marcas de eco de una sesión. Se purgan por edad en
	// flush, pero flush solo corre si el host cuenta algo: un invitado que
	// escribe rutas que nadie vigila las apuntaba para siempre. Pasado el tope
	// se deja de apuntar: lo peor es que el invitado oiga su propio cambio,
	// una invalidación de más, no un dato perdido.
	maxEcho = 8192
)

// errNoWatch: este sistema no tiene con qué vigilar.
var errNoWatch = errors.New("no host file watching on this system")

// errWatchFull: no caben más vigilancias (descriptores de kqueue, o
// max_user_watches de inotify).
var errWatchFull = errors.New("too many watched paths")

// rawEvent es lo que cuenta el sistema de un elemento vigilado.
type rawEvent struct {
	rel  string
	dir  bool // es un directorio: cambió su lista de entradas
	gone bool // se borró, se movió o se revocó
	// overflow: el sistema perdió eventos (rel no cuenta); hay que repasarlo
	// todo.
	overflow bool
}

// Aviso de una vez por daemon: se pierden eventos.
var overflowLogged atomic.Bool

// fingerprint es lo que se compara de un fichero vigilado tras perder eventos.
type fingerprint struct {
	size, mtime int64
	ino         uint64
	mode        fs.FileMode
}

func fingerprintOf(fi fs.FileInfo) fingerprint {
	f := fingerprint{size: fi.Size(), mtime: fi.ModTime().UnixNano(), mode: fi.Mode()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		f.ino = uint64(st.Ino)
	}
	return f
}

// watchBackend es kqueue o inotify. add y remove los llama el watcher con su
// cerrojo tomado; los eventos llegan por emit desde la goroutine del backend.
type watchBackend interface {
	add(rel string, dir bool) error
	remove(rel string)
	close()
}

type watchEntry struct {
	rel  string
	dir  bool
	snap map[string]bool // dir: sus nombres; nil si no se tomó
	// cost es lo que snap cuenta en watcher.snapBytes; id, el directorio real
	// de la foto (si snap no es nil y se supo).
	cost  int64
	id    Ident
	hayID bool
	// fotoDesde es cuándo se empezó a leer snap: el repaso no la relee si el
	// directorio no ha cambiado de fecha desde bastante antes (fotoFiable).
	fotoDesde time.Time
	// fp es su huella al vigilarlo o al último aviso; en un directorio sin
	// foto, lo único con lo que el repaso sabe si cambió su lista.
	fp fingerprint
	el *list.Element
	// sondeo: no tiene vigilancia del sistema (no cupo) y la repasa
	// watcher.sondear cada pollInterval.
	sondeo bool
}

// snapEntryCost es lo que se estima que ocupa en el heap cada nombre de una
// foto además de sus bytes (cabecera de la cadena, hueco del mapa).
const snapEntryCost = 48

// snapCost estima lo que ocupa la foto m.
func snapCost(m map[string]bool) int64 {
	c := int64(0)
	for n := range m {
		c += int64(len(n)) + snapEntryCost
	}
	return c
}

// watcher es el de UNA sesión: muere con ella.
type watcher struct {
	srv  *Server
	push func([]byte)
	be   watchBackend

	mu      sync.Mutex
	closed  bool
	entries map[string]*watchEntry
	lru     *list.List // *watchEntry; el más reciente delante
	// snapBytes es lo que ocupan las fotos (snapCost), con tope maxSnapBytes;
	// snapOf, qué entrada tiene la foto de cada directorio real: varios
	// nombres del mismo directorio (enlaces dentro de la carpeta) comparten
	// una sola foto, la del primero.
	snapBytes int64
	snapOf    map[Ident]*watchEntry
	echo      map[string]time.Time
	// echoSweep: última purga de echo por edad hecha desde touched.
	echoSweep time.Time
	pend      map[string]rawEvent
	timer     *time.Timer
	// resync: se perdieron eventos; el próximo flush lo repasa todo.
	resync bool
	// flushing: hay un flush en marcha; again, llegaron eventos mientras y
	// toca otro al acabar. Sin esto, cada watchDebounce con eventos era una
	// goroutine más esperando en la escritura a un invitado que no lee.
	flushing, again bool
	// polling: vigila por sondeo (pollBackend); el repaso es lo normal y no
	// se avisa de eventos perdidos.
	polling bool
	// kernel y sondeadas cuentan las entradas con vigilancia del sistema y
	// las que se vigilan por sondeo (watchEntry.sondeo). sondeoTimer repasa
	// estas cada pollInterval mientras las haya; resyncSondeo pide al
	// siguiente flush que lo haga, y sondeoAvisado es el log de la sesión.
	kernel, sondeadas int
	sondeoTimer       *time.Timer
	resyncSondeo      bool
	sondeoAvisado     bool
}

// newWatcher arranca la vigilancia de s para una sesión. push manda una trama
// al agente (la misma escritura serializada que las respuestas).
func newWatcher(s *Server, push func([]byte)) (*watcher, error) {
	w := &watcher{
		srv: s, push: push,
		entries: map[string]*watchEntry{}, lru: list.New(),
		echo: map[string]time.Time{}, pend: map[string]rawEvent{},
	}
	be, err := newWatchBackend(s.root, w.event)
	if err != nil {
		return nil, err
	}
	w.be = be
	_, w.polling = be.(*pollBackend)
	return w, nil
}

func (w *watcher) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	if w.timer != nil {
		w.timer.Stop()
	}
	if w.sondeoTimer != nil {
		w.sondeoTimer.Stop()
	}
	w.be.close()
	w.entries, w.pend, w.snapOf = nil, nil, nil
	w.snapBytes = 0
}

// room dice cuántas rutas más se pueden vigilar sin expulsar otras (con un
// margen de una cuarta parte para lo que el invitado nombre solo). Un watcher
// nulo (sesión sin OpWatch) no vigila nada y no pone tope.
func (w *watcher) room() int {
	if w == nil {
		return 1 << 30
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return maxWatchesPerSession*3/4 - len(w.entries)
}

// note registra rel como vista por el invitado (y, si es nueva, la vigila).
func (w *watcher) note(rel string, dir bool) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	if e := w.entries[rel]; e != nil {
		w.lru.MoveToFront(e.el)
		if e.dir == dir {
			return
		}
		// Cambió de tipo (se borró un fichero y se creó un directorio): de nuevo.
		w.dropLocked(e)
	}
	for len(w.entries) >= maxWatchesPerSession {
		w.dropLocked(w.lru.Back().Value.(*watchEntry))
	}
	if !w.polling && w.kernel >= maxKernelWatches {
		w.aSondeoLocked()
	}
	sondeo := false
	if err := w.be.add(rel, dir); err != nil {
		if !errors.Is(err, errWatchFull) {
			return
		}
		// No caben más vigilancias del sistema (descriptores de kqueue del
		// daemon, max_user_watches de inotify): por sondeo, en vez de
		// olvidarla. Antes sus vigilantes del invitado no disparaban nunca.
		sondeo = true
	}
	e := &watchEntry{rel: rel, dir: dir, fp: w.fingerprint(rel), sondeo: sondeo}
	e.el = w.lru.PushFront(e)
	w.entries[rel] = e
	if sondeo {
		w.sondeadaLocked()
	} else {
		w.kernel++
	}
	if !dir {
		return
	}
	// La foto se lee sin el cerrojo (son hasta 20 000 nombres de disco): los
	// eventos de la sesión no esperan a la lectura. Ya se vigila, así que lo
	// que cambie mientras tanto llega como evento; si flush lo procesa antes
	// de que esté la foto, avisa del directorio entero y la toma él.
	w.mu.Unlock()
	desde := time.Now()
	snap, id, ok := w.snapshot(rel, func(id Ident) bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.otraFotoLocked(e, id)
	})
	w.mu.Lock()
	if !w.closed && w.entries[rel] == e && e.snap == nil {
		w.setSnapLocked(e, snap, id, ok, desde)
	}
}

// setSnapLocked pone snap (leída desde desde) como foto de e, si cabe en el
// presupuesto de la sesión y ninguna otra entrada tiene ya la de ese
// directorio; si no, e se queda sin foto.
func (w *watcher) setSnapLocked(e *watchEntry, snap map[string]bool, id Ident, hayID bool, desde time.Time) {
	w.clearSnapLocked(e)
	if snap == nil {
		return
	}
	if hayID && w.otraFotoLocked(e, id) {
		return
	}
	c := snapCost(snap)
	if w.snapBytes+c > maxSnapBytes {
		return
	}
	e.snap, e.cost, e.id, e.hayID, e.fotoDesde = snap, c, id, hayID, desde
	w.snapBytes += c
	if hayID {
		if w.snapOf == nil {
			w.snapOf = map[Ident]*watchEntry{}
		}
		w.snapOf[id] = e
	}
}

// otraFotoLocked dice si otra entrada que no es e ya tiene la foto del
// directorio id.
func (w *watcher) otraFotoLocked(e *watchEntry, id Ident) bool {
	o := w.snapOf[id]
	return o != nil && o != e
}

// clearSnapLocked suelta la foto de e y devuelve su sitio al presupuesto.
func (w *watcher) clearSnapLocked(e *watchEntry) {
	if e.snap == nil {
		return
	}
	w.snapBytes -= e.cost
	if e.hayID && w.snapOf[e.id] == e {
		delete(w.snapOf, e.id)
	}
	e.snap, e.cost, e.hayID = nil, 0, false
}

func (w *watcher) dropLocked(e *watchEntry) {
	w.clearSnapLocked(e)
	if e.sondeo {
		w.sondeadas--
	} else {
		w.be.remove(e.rel)
		w.kernel--
	}
	w.lru.Remove(e.el)
	delete(w.entries, e.rel)
}

// aSondeoLocked pasa a sondeo la entrada menos reciente de las que tienen
// vigilancia del sistema, para dejar su sitio a una nueva: lo que el invitado
// acaba de mirar es lo que más probablemente vigila.
func (w *watcher) aSondeoLocked() {
	for el := w.lru.Back(); el != nil; el = el.Prev() {
		e := el.Value.(*watchEntry)
		if e.sondeo {
			continue
		}
		w.be.remove(e.rel)
		w.kernel--
		e.sondeo = true
		w.sondeadaLocked()
		return
	}
}

// sondeadaLocked cuenta una entrada más por sondeo: lo dice una vez por sesión
// y arranca el repaso si no corría.
func (w *watcher) sondeadaLocked() {
	w.sondeadas++
	if !w.sondeoAvisado {
		w.sondeoAvisado = true
		log.Printf("share %s: a session watches more paths than the host can follow (%d); the rest are checked every %s", w.srv.dir, w.kernel, pollInterval)
	}
	if w.sondeoTimer == nil {
		w.sondeoTimer = time.AfterFunc(pollInterval, w.sondear)
	}
}

// sondear pide al flush siguiente que repase las entradas por sondeo, y se
// vuelve a programar mientras las haya.
func (w *watcher) sondear() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	if w.sondeadas == 0 {
		w.sondeoTimer = nil
		return
	}
	w.resyncSondeo = true
	if w.timer == nil {
		w.timer = time.AfterFunc(watchDebounce, w.flush)
	} else {
		w.timer.Reset(watchDebounce)
	}
	w.sondeoTimer.Reset(pollInterval)
}

// touched apunta que el invitado acaba de cambiar rel: lo que el host cuente
// de ella en guestEcho es eco.
func (w *watcher) touched(rels ...string) {
	if w == nil {
		return
	}
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	// Purga por edad también aquí, como mucho una vez por guestEcho (o al
	// llegar al tope): así el mapa no guarda más que el último par de
	// segundos aunque flush no corra nunca.
	if now.Sub(w.echoSweep) > guestEcho || len(w.echo) >= maxEcho {
		w.purgeEchoLocked(now)
		w.echoSweep = now
	}
	for _, r := range rels {
		if _, ok := w.echo[r]; !ok && len(w.echo) >= maxEcho {
			continue
		}
		w.echo[r] = now
	}
}

// purgeEchoLocked suelta las marcas de eco que ya vencieron.
func (w *watcher) purgeEchoLocked(now time.Time) {
	for r, t := range w.echo {
		if now.Sub(t) > guestEcho {
			delete(w.echo, r)
		}
	}
}

// fingerprint es la huella actual de rel (cero si no se puede leer).
func (w *watcher) fingerprint(rel string) fingerprint {
	fi, err := w.srv.root.Lstat(relPath(rel))
	if err != nil {
		return fingerprint{}
	}
	return fingerprintOf(fi)
}

// snapshot lee los nombres de un directorio (nil si no se puede o es enorme)
// y dice cuál es el directorio real que leyó (rel puede pasar por enlaces). Si
// skip dice que ese directorio ya tiene foto, no lee los nombres.
func (w *watcher) snapshot(rel string, skip func(Ident) bool) (map[string]bool, Ident, bool) {
	f, err := openDir(w.srv.root, relPath(rel))
	if err != nil {
		return nil, Ident{}, false
	}
	defer f.Close()
	var id Ident
	ok := false
	if fi, err := f.Stat(); err == nil {
		if !fi.IsDir() {
			return nil, Ident{}, false
		}
		id, ok = IdentOf(fi)
		if ok && skip(id) {
			return nil, Ident{}, false
		}
	}
	names, err := f.Readdirnames(maxDirSnapshot + 1)
	if (err != nil && !errors.Is(err, io.EOF)) || len(names) > maxDirSnapshot {
		return nil, Ident{}, false
	}
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m, id, ok
}

// event lo llama el backend: se acumula y se procesa tras watchDebounce.
func (w *watcher) event(ev rawEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	if ev.overflow {
		w.resync = true
	} else if old, ok := w.pend[ev.rel]; ok {
		ev.gone = ev.gone || old.gone
		ev.dir = ev.dir || old.dir
	}
	if !ev.overflow {
		w.pend[ev.rel] = ev
	}
	if w.timer == nil {
		w.timer = time.AfterFunc(watchDebounce, w.flush)
	} else {
		w.timer.Reset(watchDebounce)
	}
}

type pushMsg struct {
	ev  byte
	rel string
}

// flush procesa lo acumulado y manda los avisos.
func (w *watcher) flush() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	if w.flushing {
		// Otro flush sigue mandando (un invitado que no lee lo deja en la
		// escritura): este no se suma, lo acumulado lo procesa aquel al acabar.
		w.again = true
		w.mu.Unlock()
		return
	}
	w.flushing = true
	defer func() {
		w.mu.Lock()
		w.flushing = false
		if w.again && !w.closed {
			w.again = false
			w.timer.Reset(watchDebounce)
		}
		w.mu.Unlock()
	}()
	pend := w.pend
	w.pend = map[string]rawEvent{}
	if w.resync {
		w.resync, w.resyncSondeo = false, false
		w.resyncLocked(pend, false)
	} else if w.resyncSondeo {
		w.resyncSondeo = false
		w.resyncLocked(pend, true)
	}
	now := time.Now()
	w.purgeEchoLocked(now)
	isEcho := func(r string) bool { t, ok := w.echo[r]; return ok && now.Sub(t) <= guestEcho }

	var out []pushMsg
	sent := map[pushMsg]bool{}
	add := func(ev byte, r string) {
		m := pushMsg{ev, r}
		if !sent[m] && !isEcho(r) {
			sent[m] = true
			out = append(out, m)
		}
	}
	for _, ev := range pend {
		e := w.entries[ev.rel]
		if e == nil {
			continue
		}
		// La huella de ahora, antes que la foto: lo que cambie entre medias se
		// ve en el repaso siguiente.
		fp := w.fingerprint(ev.rel)
		if !ev.gone && e.fp.ino != 0 && fp.ino != 0 && fp.ino != e.fp.ino {
			// En su sitio hay otro inodo (un guardado atómico) y el sistema solo
			// contó un cambio de atributos del viejo: inotify sigue el inodo,
			// no el nombre, y si alguien aún lo tiene abierto no avisa de su
			// borrado. Se trata como lo que es, otro fichero en la ruta: sin
			// esto la vigilancia se quedaba en el viejo y no avisaba de nada más.
			ev.gone = true
		}
		switch {
		case ev.gone:
			// Se borró o se movió. Si en su sitio hay otra cosa (un guardado
			// atómico: escribir aparte y renombrar encima) es un cambio, y se
			// vuelve a vigilar lo nuevo.
			w.dropLocked(e)
			if fi, err := w.srv.root.Lstat(relPath(ev.rel)); err == nil {
				d := fi.IsDir()
				w.mu.Unlock()
				w.note(ev.rel, d)
				w.mu.Lock()
				if w.closed {
					w.mu.Unlock()
					return
				}
				if !d {
					add(proto.EventModify, ev.rel)
				}
			} else if ev.rel != "" {
				add(proto.EventDelete, ev.rel)
			}
		case e.dir:
			e.fp = fp
			desde := time.Now()
			now, id, hayID := w.snapshot(ev.rel, func(id Ident) bool { return w.otraFotoLocked(e, id) })
			if e.snap == nil || now == nil {
				// Sin foto con la que comparar (directorio enorme, presupuesto
				// gastado, otro nombre del mismo directorio ya la tiene): se
				// avisa del directorio, y el invitado relee su lista.
				w.setSnapLocked(e, now, id, hayID, desde)
				if ev.rel != "" {
					add(proto.EventModify, ev.rel)
				}
				continue
			}
			for n := range now {
				if !e.snap[n] {
					add(proto.EventCreate, proto.Join(ev.rel, n))
				}
			}
			for n := range e.snap {
				if !now[n] {
					c := proto.Join(ev.rel, n)
					add(proto.EventDelete, c)
					if ce := w.entries[c]; ce != nil {
						w.dropLocked(ce)
					}
				}
			}
			w.setSnapLocked(e, now, id, hayID, desde)
		default:
			e.fp = fp
			add(proto.EventModify, ev.rel)
		}
	}
	w.mu.Unlock()
	for _, m := range out {
		w.push(proto.Push(m.ev, m.rel))
	}
}

// resyncLocked añade a pend un evento por cada ruta vigilada que pudo cambiar
// sin que se supiera: los directorios siempre (flush los compara con su foto y
// solo avisa de lo que difiera), los ficheros si su huella no es la de antes, y
// lo que ya no existe como borrado. Con soloSondeo, solo las que se vigilan
// por sondeo (watchEntry.sondeo).
func (w *watcher) resyncLocked(pend map[string]rawEvent, soloSondeo bool) {
	if !w.polling && !soloSondeo && overflowLogged.CompareAndSwap(false, true) {
		log.Printf("share %s: the host lost change events; rescanning the %d watched paths (logged once)", w.srv.dir, len(w.entries))
	}
	for r, e := range w.entries {
		if soloSondeo && !e.sondeo {
			continue
		}
		if _, ok := pend[r]; ok {
			continue
		}
		fi, err := w.srv.root.Lstat(relPath(r))
		switch {
		case err != nil || fi.IsDir() != e.dir:
			pend[r] = rawEvent{rel: r, dir: e.dir, gone: true}
		case e.dir && e.snap == nil && fingerprintOf(fi) == e.fp:
			// Sin foto, flush avisaría del directorio entero: por sondeo, cada
			// pollInterval y para siempre (vite o nodemon recompilando en
			// bucle). Si su fecha y su tamaño no cambiaron, su lista tampoco.
		case e.dir && fingerprintOf(fi) == e.fp && fotoFiable(e, fi):
			// Con foto, lo mismo: releerla era leer hasta 20 000 nombres de
			// cada directorio vigilado, con w.mu tomado, cada pollInterval.
		case e.dir:
			pend[r] = rawEvent{rel: r, dir: true}
		case fingerprintOf(fi) != e.fp:
			pend[r] = rawEvent{rel: r}
		}
	}
}

// margenFecha es lo que se desconfía de la fecha de un directorio: en un
// sistema de ficheros con fechas gruesas (HFS+ por segundos, FAT por dos, un
// SMB) un cambio en el mismo tic que la foto no mueve la fecha.
const margenFecha = 2 * time.Second

// fotoFiable dice si la foto de e vale mientras la fecha del directorio (fi)
// no cambie: se empezó a leer más de margenFecha después de esa fecha, así que
// cualquier cambio posterior la habría movido. Si no, se relee, y la foto
// nueva ya lo es.
func fotoFiable(e *watchEntry, fi fs.FileInfo) bool {
	return e.fotoDesde.Sub(fi.ModTime()) > margenFecha
}

// relPath es rel para os.Root ("" es la raíz).
func relPath(p string) string { return rel(p) }
