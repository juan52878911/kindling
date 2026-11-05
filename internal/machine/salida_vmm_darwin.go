//go:build darwin

package machine

import (
	"errors"
	"sync"
	"syscall"
)

// vigiaKqueue espera a la salida de procesos que no son hijos de este daemon
// (los VMM readoptados) con un solo kqueue y una sola goroutine para todos:
// EVFILT_PROC con NOTE_EXIT, que el kernel entrega una vez por proceso.
var vigiaKqueue struct {
	once sync.Once
	kq   int
	err  error
	mu   sync.Mutex
	fns  map[int][]func()
}

// vigilarSalida llama a fn (en su propia goroutine) cuando termine el
// proceso pid, o enseguida si ya no existe. Dice si sabe vigilarlo. Nunca
// llama a fn antes de volver: se puede usar con candados tomados.
func vigilarSalida(pid int, fn func()) bool {
	v := &vigiaKqueue
	v.once.Do(func() {
		v.kq, v.err = syscall.Kqueue()
		if v.err != nil {
			return
		}
		syscall.CloseOnExec(v.kq)
		v.fns = map[int][]func(){}
		go bucleKqueue()
	})
	if v.err != nil || pid <= 0 {
		return false
	}
	v.mu.Lock()
	v.fns[pid] = append(v.fns[pid], fn)
	v.mu.Unlock()
	ev := syscall.Kevent_t{
		Ident:  uint64(pid),
		Filter: syscall.EVFILT_PROC,
		Flags:  syscall.EV_ADD | syscall.EV_ONESHOT,
		Fflags: syscall.NOTE_EXIT,
	}
	if _, err := syscall.Kevent(v.kq, []syscall.Kevent_t{ev}, nil, nil); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			// Ya terminó: se avisa igual, que es lo que importa.
			dispararSalida(pid)
			return true
		}
		v.mu.Lock()
		delete(v.fns, pid)
		v.mu.Unlock()
		return false
	}
	return true
}

func bucleKqueue() {
	evs := make([]syscall.Kevent_t, 16)
	for {
		n, err := syscall.Kevent(vigiaKqueue.kq, nil, evs, nil)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return
		}
		for _, ev := range evs[:n] {
			if ev.Filter == syscall.EVFILT_PROC && ev.Fflags&syscall.NOTE_EXIT != 0 {
				dispararSalida(int(ev.Ident))
			}
		}
	}
}

// dispararSalida lanza los avisos apuntados para pid y los retira.
func dispararSalida(pid int) {
	v := &vigiaKqueue
	v.mu.Lock()
	fns := v.fns[pid]
	delete(v.fns, pid)
	v.mu.Unlock()
	for _, fn := range fns {
		go fn()
	}
}
