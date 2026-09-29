//go:build unix

package dbstate

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// openLockFile abre (o crea, 0600) el fichero del cerrojo sin seguir enlaces y
// exige que sea un fichero normal de este usuario.
func openLockFile(p string) (*os.File, error) {
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the lock %s: %w", p, err)
	}
	st, err := f.Stat()
	if err == nil && !st.Mode().IsRegular() {
		err = errors.New("not a regular file")
	}
	if err == nil {
		err = ownedByUs(st)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", p, err)
	}
	return f, nil
}

// tryLock intenta el flock exclusivo sin bloquear: false si está ocupado.
func tryLock(f *os.File) (bool, error) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return false, nil
		}
		return false, fmt.Errorf("flock %s: %w", f.Name(), err)
	}
}

func unlock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
