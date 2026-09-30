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
		Dev: uint64(st.Dev), Ino: st.Ino, Size: st.Size, Mode: uint32(st.Mode), UID: st.Uid,
		Mtime: st.Mtimespec.Nano(), Ctime: st.Ctimespec.Nano(),
	}, true
}
