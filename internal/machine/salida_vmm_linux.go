//go:build linux

package machine

import (
	"errors"
	"sync"
	"syscall"
)

// sysPidfdOpen es pidfd_open(2): el mismo número en todas las arquitecturas
// (Linux 5.3+). El paquete syscall no lo trae.
const sysPidfdOpen = 434

// vigiaPidfd espera a la salida de procesos que no son hijos de este daemon
// (los VMM readoptados) con un pidfd por proceso y un solo epoll para todos:
// el pidfd se vuelve legible cuando el proceso termina.
var vigiaPidfd struct {
	once sync.Once
	ep   int
	err  error
	mu   sync.Mutex
	fns  map[int32]func() // por pidfd
}

// vigilarSalida llama a fn (en su propia goroutine) cuando termine el
// proceso pid, o enseguida si ya no existe. Dice si sabe vigilarlo (sin
// pidfd, kernel anterior a 5.3, no). Nunca llama a fn antes de volver: se
// puede usar con candados tomados.
func vigilarSalida(pid int, fn func()) bool {
	v := &vigiaPidfd
	v.once.Do(func() {
		v.ep, v.err = syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
		if v.err != nil {
			return
		}
		v.fns = map[int32]func(){}
		go bucleEpoll()
	})
	if v.err != nil || pid <= 0 {
		return false
	}
	r, _, e := syscall.Syscall(sysPidfdOpen, uintptr(pid), 0, 0)
	if e != 0 {
		if errors.Is(e, syscall.ESRCH) {
			go fn()
			return true
		}
		return false
	}
	fd := int(r)
	syscall.CloseOnExec(fd)
	v.mu.Lock()
	v.fns[int32(fd)] = fn
	v.mu.Unlock()
	ev := syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(fd)}
	if err := syscall.EpollCtl(v.ep, syscall.EPOLL_CTL_ADD, fd, &ev); err != nil {
		v.mu.Lock()
		delete(v.fns, int32(fd))
		v.mu.Unlock()
		syscall.Close(fd)
		return false
	}
	return true
}

func bucleEpoll() {
	v := &vigiaPidfd
	evs := make([]syscall.EpollEvent, 16)
	for {
		n, err := syscall.EpollWait(v.ep, evs, -1)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return
		}
		for _, ev := range evs[:n] {
			fd := ev.Fd
			_ = syscall.EpollCtl(v.ep, syscall.EPOLL_CTL_DEL, int(fd), nil)
			v.mu.Lock()
			fn := v.fns[fd]
			delete(v.fns, fd)
			v.mu.Unlock()
			syscall.Close(int(fd))
			if fn != nil {
				go fn()
			}
		}
	}
}
