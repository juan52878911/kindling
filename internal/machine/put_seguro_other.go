//go:build !linux

package machine

import (
	"errors"
	"os"
)

// ponerEnImagen solo existe en Linux (put_seguro_linux.go): en macOS
// intentarPut no monta y va por debugfs (put_debugfs.go).
func ponerEnImagen(mnt, dentroPath, src, quiero string, mode os.FileMode, create bool) (bool, error) {
	return false, errors.New("writing into a mounted image is only supported on Linux")
}
