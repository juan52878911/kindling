package dbstate

// Cerrojos entre procesos de kling-db en este host: un fichero vacío por
// cerrojo en locks/ del estado, con flock(2) exclusivo. El sistema lo suelta
// si el proceso muere, así que no queda nada colgado que limpiar.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// LocksDir es el subdirectorio de los cerrojos.
const LocksDir = "locks"

// lockPattern es un nombre de cerrojo: lo único que entra en la ruta.
var lockPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// ErrLocked: otro proceso tiene el cerrojo y no lo soltó a tiempo.
var ErrLocked = errors.New("locked by another kling db process")

// lockPoll es cada cuánto se reintenta un cerrojo ocupado.
const lockPoll = 50 * time.Millisecond

// Lock toma el cerrojo name. Si está ocupado, reintenta hasta wait (0: un solo
// intento) o hasta que ctx acabe; waiting, si no es nil, se llama una vez al
// empezar a esperar (para avisar). Devuelve la función que lo suelta.
func Lock(ctx context.Context, name string, wait time.Duration, waiting func()) (func(), error) {
	if !lockPattern.MatchString(name) {
		return nil, fmt.Errorf("invalid lock name %q", name)
	}
	root, err := Dir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, LocksDir)
	for _, d := range []string{root, dir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return nil, err
		}
	}
	f, err := openLockFile(filepath.Join(dir, name+".lock"))
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	told := false
	for {
		ok, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if ok {
			return func() { unlock(f); f.Close() }, nil
		}
		if !time.Now().Before(deadline) {
			f.Close()
			return nil, ErrLocked
		}
		if !told && waiting != nil {
			waiting()
			told = true
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}
