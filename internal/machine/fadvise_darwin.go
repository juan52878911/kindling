package machine

import "os"

// dropCache no hace nada en macOS: no hay POSIX_FADV_DONTNEED, y la caché
// unificada suelta las páginas limpias bajo presión. precargar sí corre aquí
// (precarga.go).
func dropCache(path string) {}

// perforarRango es nil en macOS: el mem.file de kling-vz/kling-hv no es un
// volcado crudo de la RAM con páginas a cero (perforarHuecos tampoco hace
// nada), y paseUnico se queda en digest.Tree.
var perforarRango func(f *os.File, off, n int64) error
