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
)

// marcado devuelve un error con el texto de format que además "es" marca.
func marcado(marca error, format string, a ...any) error {
	return &errMarcado{fmt.Sprintf(format, a...), marca}
}
