//go:build !unix

package plugin

import (
	"errors"
	"os"
)

// Fuera de unix no se sabe de quién es un fichero: la caché no se lee.
func ownedByMe(os.FileInfo) bool { return false }

func openNoFollow(string) (*os.File, error) { return nil, errors.ErrUnsupported }

// Fuera de unix no hay dueño ni modo que mirar: kling no se publica para esos
// sistemas y se confía en el PATH como hasta ahora.
func trustedExecutable(dir, path string) error { return nil }
