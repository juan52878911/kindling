package upgrade

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// bloquear toma el cerrojo de dir/.lock para esta actualización o vuelta
// atrás, sin esperar. Dos `kling upgrade` sobre la misma raíz (dos personas,
// o un script y una persona) comparten el directorio de descarga, que cada uno
// borra al acabar, y paran y arrancan el mismo servicio: uno borraría lo que
// baja el otro o volvería atrás encima de su actualización. El kernel suelta
// el cerrojo si el proceso muere, también con un SIGKILL.
func bloquear(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ruta := filepath.Join(dir, ".lock")
	f, err := os.OpenFile(ruta, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another kling upgrade is running on %s (%s is locked): wait for it to finish", dir, ruta)
		}
		return nil, fmt.Errorf("locking %s: %w", ruta, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
