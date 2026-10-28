package machine

import (
	"errors"
	"fmt"
)

// Centinelas de imágenes y snapshots. El daemon las traduce a código HTTP con
// errors.Is (400, 404, 409) en vez de buscar "does not exist" en el texto, que
// cambiaba de significado con cualquier mensaje que lo contuviera. El texto
// que ve el usuario es el de siempre: van marcadas con errMarcado (exec.go).
var (
	// ErrInvalidName: el nombre de imagen o snapshot no pasa validName.
	ErrInvalidName = errors.New("invalid name")
	// ErrNoImage: no hay imagen con ese nombre.
	ErrNoImage = errors.New("image does not exist")
	// ErrImageInUse: la imagen es base de otra capa, la usa un dorado o una
	// máquina viva; borrarla dejaría algo sin rootfs.
	ErrImageInUse = errors.New("image is in use")
	// ErrNoSnapshot: no hay snapshot con ese nombre.
	ErrNoSnapshot = errors.New("snapshot does not exist")
	// ErrSnapshotInUse: el snapshot tiene instancias vivas, o un commit o una
	// restauración lo tiene reservado.
	ErrSnapshotInUse = errors.New("snapshot is in use")
	// ErrRunRequest: lo que está mal es la petición (un on_ttl que no existe,
	// un disk_mib fuera de rango, mem_max_mib por debajo de mem_mib...), no el
	// host. El daemon la contesta con 400; lo que no lleva marca es del host
	// y sale como 500 (M13).
	ErrRunRequest = errors.New("invalid machine request")
)

// peticionInvalida es un error de la petición con el texto de format.
func peticionInvalida(format string, a ...any) error { return marcado(ErrRunRequest, format, a...) }

// comoPeticion marca err como error de la petición sin cambiar su texto.
func comoPeticion(err error) error {
	if err == nil {
		return nil
	}
	return &errMarcado{err.Error(), ErrRunRequest}
}

// marcado devuelve un error con el texto de format que además "es" marca.
func marcado(marca error, format string, a ...any) error {
	return &errMarcado{fmt.Sprintf(format, a...), marca}
}
