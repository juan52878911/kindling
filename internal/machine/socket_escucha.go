package machine

import (
	"errors"
	"net"
	"os"
	"syscall"
	"time"
)

// nadieEscuchaEn dice si es seguro que en el socket unix sock no escucha
// nadie: no existe o rechaza la conexión. Un error de otro tipo no lo
// asegura (false).
func nadieEscuchaEn(sock string) bool {
	// Sin el fichero no hay nadie, y sin marcar: un socket con una ruta que
	// no cabe en sun_path daría EINVAL al marcar, no concluyente.
	if _, err := os.Lstat(sock); errors.Is(err, os.ErrNotExist) {
		return true
	}
	c, err := net.DialTimeout("unix", sock, 100*time.Millisecond)
	if err == nil {
		c.Close()
		return false
	}
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}
