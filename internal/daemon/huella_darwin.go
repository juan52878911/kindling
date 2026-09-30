package daemon

import (
	"os"
	"syscall"
)

// identidadFichero es el inodo y el ctime (ns) de fi: ver huellaBlob.
func identidadFichero(fi os.FileInfo) (ino uint64, ctimeNS int64) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return uint64(st.Ino), st.Ctimespec.Sec*1e9 + st.Ctimespec.Nsec
}
