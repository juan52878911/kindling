package plugin

import (
	"os"
	"syscall"
)

func fileIDOf(fi os.FileInfo) (fileID, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}, false
	}
	return fileID{
		Dev: uint64(st.Dev), Ino: uint64(st.Ino), Size: st.Size, Mode: uint32(st.Mode), UID: st.Uid,
		Mtime: st.Mtim.Nano(), Ctime: st.Ctim.Nano(),
	}, true
}
