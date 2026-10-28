//go:build linux

package share

// Vigilancia con inotify. La ruta se abre por os.Root (O_PATH) y se vigila
// por /proc/self/fd/N: así un enlace simbólico no lleva la vigilancia fuera de
// la carpeta, y no queda ningún descriptor abierto por ruta.

import (
	"errors"
	"os"
	"strconv"
	"sync"
	"syscall"
	"unsafe"
)

const (
	inDirMask  = syscall.IN_CREATE | syscall.IN_DELETE | syscall.IN_MOVED_FROM | syscall.IN_MOVED_TO | syscall.IN_DELETE_SELF | syscall.IN_MOVE_SELF | syscall.IN_ONLYDIR
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
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
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
	h, err := b.root.OpenFile(relPath(rel), syscall.O_PATH, 0)
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
