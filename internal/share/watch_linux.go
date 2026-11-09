//go:build linux

package share

// Vigilancia con inotify. La ruta se abre por os.Root (O_PATH) y se vigila
// por /proc/self/fd/N: así un enlace simbólico no lleva la vigilancia fuera de
// la carpeta, y no queda ningún descriptor abierto por ruta.

import (
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// Presupuesto de instancias de inotify de todo el daemon. Hay una por sesión,
// y el kernel no las cuenta por proceso sino por uid
// (fs.inotify.max_user_instances, 128 por defecto): sin tope, unas decenas de
// carpetas dejaban sin inotify al resto de procesos del mismo usuario (root,
// si el daemon es root). Pasado el tope, la sesión vigila por sondeo
// (pollBackend), como el presupuesto watchFDs de macOS pero sin quedarse sin
// avisos.
var inotifyBudget struct {
	sync.Mutex
	used, max int // max 0: aún sin calcular (maxInotifyInstances)
}

// pollLogged: se dice una vez que se vigila por sondeo.
var pollLogged atomic.Bool

// maxInotifyInstances es la mitad de max_user_instances, entre 1 y 512 (64
// si no se puede leer): la otra mitad queda para los demás procesos del uid.
func maxInotifyInstances() int {
	b, err := os.ReadFile("/proc/sys/fs/inotify/max_user_instances")
	if err != nil {
		return 64
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 64
	}
	return min(max(n/2, 1), 512)
}

func takeInotify() bool {
	inotifyBudget.Lock()
	defer inotifyBudget.Unlock()
	if inotifyBudget.max == 0 {
		inotifyBudget.max = maxInotifyInstances()
	}
	if inotifyBudget.used >= inotifyBudget.max {
		return false
	}
	inotifyBudget.used++
	return true
}

func releaseInotify() {
	inotifyBudget.Lock()
	inotifyBudget.used--
	inotifyBudget.Unlock()
}

// pollFallback es la vigilancia por sondeo cuando no queda inotify.
func pollFallback(why string, emit func(rawEvent)) watchBackend {
	if pollLogged.CompareAndSwap(false, true) {
		log.Printf("share: %s; host changes reach the guest by polling every %s (logged once)", why, pollInterval)
	}
	return newPollBackend(emit)
}

const (
	inDirMask = syscall.IN_CREATE | syscall.IN_DELETE | syscall.IN_MOVED_FROM | syscall.IN_MOVED_TO | syscall.IN_DELETE_SELF | syscall.IN_MOVE_SELF | syscall.IN_ONLYDIR
	// oPath es O_PATH (0x200000 en todas las arquitecturas Linux de Go): el
	// paquete syscall no lo define en todas ni en las versiones viejas de Go.
	oPath      = 0x200000
	inFileMask = syscall.IN_MODIFY | syscall.IN_ATTRIB | syscall.IN_CLOSE_WRITE | syscall.IN_DELETE_SELF | syscall.IN_MOVE_SELF
)

type inBackend struct {
	root *os.Root
	f    *os.File
	fd   int
	emit func(rawEvent)

	mu     sync.Mutex
	closed bool
	byWD   map[int32]*inWatch
	byRel  map[string]*inWatch
}

type inWatch struct {
	wd  int32
	rel string
	dir bool
}

func newWatchBackend(root *os.Root, emit func(rawEvent)) (watchBackend, error) {
	if !takeInotify() {
		return pollFallback("the daemon's inotify budget is used up", emit), nil
	}
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		releaseInotify()
		if errors.Is(err, syscall.EMFILE) {
			// max_user_instances: otros procesos del uid se las llevaron.
			return pollFallback("no inotify instances left for this user (fs.inotify.max_user_instances)", emit), nil
		}
		return nil, err
	}
	// No bloqueante y por os.File: el lector espera en el poller de Go y
	// Close lo despierta.
	b := &inBackend{root: root, f: os.NewFile(uintptr(fd), "inotify"), fd: fd, emit: emit,
		byWD: map[int32]*inWatch{}, byRel: map[string]*inWatch{}}
	go b.loop()
	return b, nil
}

func (b *inBackend) add(rel string, dir bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errNoWatch
	}
	if _, ok := b.byRel[rel]; ok {
		return nil
	}
	h, err := b.root.OpenFile(relPath(rel), oPath, 0)
	if err != nil {
		return err
	}
	defer h.Close()
	mask := uint32(inFileMask)
	if dir {
		mask = inDirMask
	}
	wd, err := syscall.InotifyAddWatch(b.fd, "/proc/self/fd/"+strconv.Itoa(int(h.Fd())), mask)
	if err != nil {
		if errors.Is(err, syscall.ENOSPC) {
			return errWatchFull // fs.inotify.max_user_watches
		}
		return err
	}
	w := &inWatch{wd: int32(wd), rel: rel, dir: dir}
	if old := b.byWD[w.wd]; old != nil {
		// El mismo inodo con dos nombres (un enlace duro): manda el último.
		delete(b.byRel, old.rel)
	}
	b.byWD[w.wd], b.byRel[rel] = w, w
	return nil
}

func (b *inBackend) remove(rel string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	w := b.byRel[rel]
	if w == nil {
		return
	}
	delete(b.byRel, rel)
	delete(b.byWD, w.wd)
	_, _ = syscall.InotifyRmWatch(b.fd, uint32(w.wd))
}

func (b *inBackend) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	_ = b.f.Close()
	releaseInotify()
}

func (b *inBackend) loop() {
	buf := make([]byte, 64<<10)
	for {
		n, err := b.f.Read(buf)
		if err != nil {
			if errors.Is(err, os.ErrClosed) {
				return
			}
			b.mu.Lock()
			closed := b.closed
			b.mu.Unlock()
			if closed {
				return
			}
			continue
		}
		var out []rawEvent
		b.mu.Lock()
		for off := 0; off+syscall.SizeofInotifyEvent <= n; {
			ev := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[off]))
			off += syscall.SizeofInotifyEvent + int(ev.Len)
			if ev.Mask&syscall.IN_Q_OVERFLOW != 0 {
				// La cola del kernel se llenó y se perdieron eventos (wd -1):
				// que el watcher lo repase todo.
				out = append(out, rawEvent{overflow: true})
				continue
			}
			w := b.byWD[ev.Wd]
			if w == nil {
				continue
			}
			if ev.Mask&syscall.IN_IGNORED != 0 {
				delete(b.byWD, w.wd)
				delete(b.byRel, w.rel)
				out = append(out, rawEvent{rel: w.rel, dir: w.dir, gone: true})
				continue
			}
			gone := ev.Mask&(syscall.IN_DELETE_SELF|syscall.IN_MOVE_SELF) != 0
			out = append(out, rawEvent{rel: w.rel, dir: w.dir, gone: gone})
		}
		b.mu.Unlock()
		for _, e := range out {
			b.emit(e)
		}
	}
}
