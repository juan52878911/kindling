//go:build !linux && !darwin

package daemon

import "os"

// identidadFichero: sin inodo ni ctime portables, la huella se queda en
// tamaño y mtime.
func identidadFichero(fi os.FileInfo) (ino uint64, ctimeNS int64) { return 0, 0 }
