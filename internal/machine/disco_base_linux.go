package machine

import (
	"os"
	"syscall"
)

// identidadDisco es el dispositivo, el inodo y el ctime (ns) de fi: ver
// huellaDisco.
func identidadDisco(fi os.FileInfo) (dev, ino uint64, ctimeNS int64) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0
	}
	return uint64(st.Dev), uint64(st.Ino), st.Ctim.Sec*1e9 + st.Ctim.Nsec
}
