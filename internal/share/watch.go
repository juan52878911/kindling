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

import (
	"container/list"
	"errors"
	"io"
	"sync"
	"time"

	proto "github.com/juan52878911/kindling/pkg/share"
)

var (
	// maxWatchesPerSession acota las rutas vigiladas por conexión.
	maxWatchesPerSession = 4096
	// maxDirSnapshot: un directorio con más entradas no se compara entrada a
	// entrada (se avisa del directorio, no de cada hijo).
	maxDirSnapshot = 20000
	// watchDebounce agrupa ráfagas (un editor que guarda escribe varias veces).
	watchDebounce = 15 * time.Millisecond
	// guestEcho: cuánto tiempo se considera eco un cambio en una ruta que tocó
	// el invitado.
	guestEcho = time.Second
)

// errNoWatch: este sistema no tiene con qué vigilar.
var errNoWatch = errors.New("no host file watching on this system")

// rawEvent es lo que cuenta el sistema de un elemento vigilado.
type rawEvent struct {
	rel  string
	dir  bool // es un directorio: cambió su lista de entradas
	gone bool // se borró, se movió o se revocó
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
	el   *list.Element
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
	echo    map[string]time.Time
	pend    map[string]rawEvent
	timer   *time.Timer
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
	w.be.close()
	w.entries, w.pend = nil, nil
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
	if err := w.be.add(rel, dir); err != nil {
		return
	}
	e := &watchEntry{rel: rel, dir: dir}
	if dir {
		e.snap = w.snapshot(rel)
	}
	e.el = w.lru.PushFront(e)
	w.entries[rel] = e
}

func (w *watcher) dropLocked(e *watchEntry) {
	w.be.remove(e.rel)
	w.lru.Remove(e.el)
	delete(w.entries, e.rel)
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
	for _, r := range rels {
		w.echo[r] = now
	}
}

// snapshot lee los nombres de un directorio (nil si no se puede o es enorme).
func (w *watcher) snapshot(rel string) map[string]bool {
	f, err := w.srv.root.Open(relPath(rel))
	if err != nil {
		return nil
	}
	defer f.Close()
	names, err := f.Readdirnames(maxDirSnapshot + 1)
	if (err != nil && !errors.Is(err, io.EOF)) || len(names) > maxDirSnapshot {
		return nil
	}
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// event lo llama el backend: se acumula y se procesa tras watchDebounce.
func (w *watcher) event(ev rawEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	if old, ok := w.pend[ev.rel]; ok {
		ev.gone = ev.gone || old.gone
		ev.dir = ev.dir || old.dir
	}
	w.pend[ev.rel] = ev
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
	pend := w.pend
	w.pend = map[string]rawEvent{}
	now := time.Now()
	for r, t := range w.echo {
		if now.Sub(t) > guestEcho {
			delete(w.echo, r)
		}
	}
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
			now := w.snapshot(ev.rel)
			if e.snap == nil || now == nil {
				e.snap = now
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
			e.snap = now
		default:
			add(proto.EventModify, ev.rel)
		}
	}
	w.mu.Unlock()
	for _, m := range out {
		w.push(proto.Push(m.ev, m.rel))
	}
}

// relPath es rel para os.Root ("" es la raíz).
func relPath(p string) string { return rel(p) }
