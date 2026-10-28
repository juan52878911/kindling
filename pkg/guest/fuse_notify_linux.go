//go:build linux

package guest

import (
	"runtime"
	"syscall"

	"github.com/juan52878911/kindling/pkg/share"
)

// lockThread ata el notificador a su hilo para siempre: su id es el que
// reconoce synthReply en las peticiones FUSE.
func lockThread() { runtime.LockOSThread() }

func gettid() uint32 { return uint32(syscall.Gettid()) }

// touchInGuest hace la operación que dispara inotify (ver fuse_notify.go).
// Los errores no importan: en el peor caso no hay evento, como antes.
func touchInGuest(op synthOp, full string) {
	switch op.ev {
	case share.EventModify:
		var st syscall.Stat_t
		if syscall.Stat(full, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG {
			return
		}
		_ = syscall.Truncate(full, st.Size)
	case share.EventDelete:
		if op.dir {
			_ = syscall.Rmdir(full)
		} else {
			_ = syscall.Unlink(full)
		}
	case share.EventCreate:
		if op.dir {
			_ = syscall.Mkdir(full, 0o755)
			return
		}
		fd, err := syscall.Open(full, syscall.O_CREAT|syscall.O_EXCL|syscall.O_RDONLY|syscall.O_CLOEXEC, 0o644)
		if err == nil {
			_ = syscall.Close(fd)
		}
	}
}
