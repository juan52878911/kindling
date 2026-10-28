//go:build darwin

package share

// Vigilancia con kqueue: un descriptor O_EVTONLY por ruta (no impide
// desmontar ni cuenta como lector) y EVFILT_VNODE. FSEvents sería un solo
// flujo por carpeta, pero necesita cgo y el núcleo se compila sin él.

import (
	"errors"
	"os"
	"sync"
	"syscall"
	"time"
)

// watchFDs acota los descriptores de vigilancia de todo el daemon, aparte de
// los de ficheros abiertos (globalFDs): vigilar no debe dejar sin descriptores
// a las operaciones.
var watchFDs = make(chan struct{}, 4096)

const kqNotes = syscall.NOTE_WRITE | syscall.NOTE_EXTEND | syscall.NOTE_ATTRIB |
	syscall.NOTE_DELETE | syscall.NOTE_RENAME | syscall.NOTE_REVOKE

type kqWatch struct {
	f   *os.File
	rel string
	dir bool
}

type kqBackend struct {
	root *os.Root
	kq   int
	emit func(rawEvent)

	mu     sync.Mutex
	closed bool
	byFD   map[int]*kqWatch
	byRel  map[string]*kqWatch
}

func newWatchBackend(root *os.Root, emit func(rawEvent)) (watchBackend, error) {
	kq, err := syscall.Kqueue()
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(kq)
	b := &kqBackend{root: root, kq: kq, emit: emit, byFD: map[int]*kqWatch{}, byRel: map[string]*kqWatch{}}
	go b.loop()
	return b, nil
}

var errWatchFull = errors.New("too many watched paths")

func (b *kqBackend) add(rel string, dir bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errNoWatch
	}
	if _, ok := b.byRel[rel]; ok {
		return nil
	}
	select {
	case watchFDs <- struct{}{}:
	default:
		return errWatchFull
	}
	f, err := b.root.OpenFile(relPath(rel), syscall.O_EVTONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		<-watchFDs
		return err
	}
	fd := int(f.Fd())
	ev := syscall.Kevent_t{Ident: uint64(fd), Filter: syscall.EVFILT_VNODE,
		Flags: syscall.EV_ADD | syscall.EV_CLEAR, Fflags: kqNotes}
	if _, err := syscall.Kevent(b.kq, []syscall.Kevent_t{ev}, nil, nil); err != nil {
		_ = f.Close()
		<-watchFDs
		return err
	}
	w := &kqWatch{f: f, rel: rel, dir: dir}
	b.byFD[fd], b.byRel[rel] = w, w
	return nil
}

func (b *kqBackend) remove(rel string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.removeLocked(rel)
}

func (b *kqBackend) removeLocked(rel string) {
	w := b.byRel[rel]
	if w == nil {
		return
	}
	delete(b.byRel, rel)
	delete(b.byFD, int(w.f.Fd()))
	// Cerrar el descriptor quita su filtro del kqueue.
	_ = w.f.Close()
	<-watchFDs
}

func (b *kqBackend) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for r := range b.byRel {
		b.removeLocked(r)
	}
	// El bucle lo ve en su siguiente vuelta (como mucho kqPoll) y cierra kq.
}

// kqPoll es cada cuánto mira el bucle si se cerró.
const kqPoll = 250 * time.Millisecond

func (b *kqBackend) loop() {
	evs := make([]syscall.Kevent_t, 64)
	ts := syscall.NsecToTimespec(int64(kqPoll))
	for {
		n, err := syscall.Kevent(b.kq, nil, evs, &ts)
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			_ = syscall.Close(b.kq)
			return
		}
		var out []rawEvent
		for i := 0; err == nil && i < n; i++ {
			w := b.byFD[int(evs[i].Ident)]
			if w == nil {
				continue
			}
			gone := evs[i].Fflags&(syscall.NOTE_DELETE|syscall.NOTE_RENAME|syscall.NOTE_REVOKE) != 0
			if w.dir && !gone && evs[i].Fflags&syscall.NOTE_WRITE == 0 {
				continue // atributos del directorio: su lista no cambió
			}
			out = append(out, rawEvent{rel: w.rel, dir: w.dir, gone: gone})
		}
		b.mu.Unlock()
		for _, e := range out {
			b.emit(e)
		}
		if err != nil && err != syscall.EINTR {
			time.Sleep(kqPoll)
		}
	}
}
