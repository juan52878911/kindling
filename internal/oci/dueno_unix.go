//go:build !windows

package oci

import (
	"os"
	"syscall"
)

// duenoRoot dice si el fichero es de root.
func duenoRoot(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0
}
