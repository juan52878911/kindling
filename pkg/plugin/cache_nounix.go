//go:build !unix

package plugin

import (
	"errors"
	"os"
)

// Fuera de unix no se sabe de quién es un fichero: la caché no se lee.
func ownedByMe(os.FileInfo) bool { return false }

func openNoFollow(string) (*os.File, error) { return nil, errors.ErrUnsupported }
