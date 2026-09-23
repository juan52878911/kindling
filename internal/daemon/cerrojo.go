package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// bloquearRaiz toma un cerrojo exclusivo sobre $root/daemon.lock y lo retiene
// mientras viva el proceso (el fichero queda abierto; el kernel lo suelta al
// morir, también con un SIGKILL).
//
// Dos daemons sobre la misma raíz se destruyen el uno al otro: cada uno ve los
// VMM del otro como huérfanos ("not registered") y los mata a los pocos
// segundos, y el segundo borra el socket del primero y se queda con él. En
// Linux systemd suele evitarlo; en el Mac basta con un `kling daemon` a mano
// con el agente de launchd ya corriendo. Se toma ANTES de crear el Manager,
// porque su reconcile de arranque ya mata lo que no reconoce.
func bloquearRaiz(root string) (*os.File, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(root, "daemon.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another kling daemon is already running on %s (%s is locked); "+
				"stop it first, or use a different -root", root, path)
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	// El PID solo informa a quien mire el fichero; el cerrojo es lo que manda.
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)
	return f, nil
}
