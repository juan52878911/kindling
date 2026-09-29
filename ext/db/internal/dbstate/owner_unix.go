//go:build unix

package dbstate

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// ownedByUs exige que el fichero sea del usuario que ejecuta.
func ownedByUs(st fs.FileInfo) error {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(s.Uid) != os.Getuid() {
		return errors.New("owned by another user")
	}
	return nil
}
