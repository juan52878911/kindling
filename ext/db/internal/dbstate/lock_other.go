//go:build !unix

package dbstate

import "os"

// Fuera de unix no hay flock: el cerrojo no excluye (kling-db solo se publica
// para Linux y macOS).
func openLockFile(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
}

func tryLock(*os.File) (bool, error) { return true, nil }

func unlock(*os.File) {}
