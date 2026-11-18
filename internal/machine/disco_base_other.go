//go:build !linux && !darwin

package machine

import "os"

// identidadDisco no tiene inodo ni ctime fuera de Linux y macOS: la huella
// queda en tamaño y fecha.
func identidadDisco(os.FileInfo) (dev, ino uint64, ctimeNS int64) { return 0, 0, 0 }
