//go:build !linux && !darwin

package plugin

import "os"

// Sin ctime conocido no hay forma segura de saber que el binario es el mismo:
// en estos sistemas no se usa la caché.
func fileIDOf(os.FileInfo) (fileID, bool) { return fileID{}, false }
