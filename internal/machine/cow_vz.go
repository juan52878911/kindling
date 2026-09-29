//go:build darwin

package machine

// Copias de disco en macOS (ver cow.go): copiarDisco ya clona con clonefile
// cuando la raíz es APFS (`cp -c`), así que no hay almacén ni jail.

import (
	"errors"
	"os"
	"path/filepath"
)

// clonarFichero no se usa en macOS: el modo reflink es de Linux.
func clonarFichero(src, dst string) error {
	return errors.New("FICLONE is Linux-only")
}

// clonarDescriptor: ídem.
func clonarDescriptor(in, out *os.File) error {
	return errors.New("FICLONE is Linux-only")
}

// Para lseek (copiarDisperso). En macOS van al revés que en Linux (allí están
// en fadvise_linux.go).
const (
	seekHole = 3 // SEEK_HOLE
	seekData = 4 // SEEK_DATA
)

// nuevoAlmacen: sin almacén en macOS.
func nuevoAlmacen(root string, priv *Privileges) *almacenCoW { return nil }

// detectarCoW: clonefile si la raíz es APFS; si no, copia.
func (m *Manager) detectarCoW(cfg CoWConfig) (string, string) {
	switch {
	case cfg.Mode == CoWOff:
		// cp -c sigue clonando en APFS; "off" solo dice que no se pidió nada.
		return cowModoCopy, "daemon.cow is off"
	case esAPFS(m.root):
		return cowModoClone, "APFS: overlays are cloned with clonefile"
	}
	return cowModoCopy, "the data root is not APFS: copying overlays"
}

func (m *Manager) prepararBindsJail(id string) error { return nil }

func (m *Manager) borrarJail(id string) error {
	return os.RemoveAll(filepath.Join(m.jailBase(), "firecracker", id))
}

func (m *Manager) barrerBindsJail() {}
