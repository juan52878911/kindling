package machine

// dropCache no hace nada en macOS: no hay POSIX_FADV_DONTNEED, y la caché
// unificada suelta las páginas limpias bajo presión. precargar sí corre aquí
// (precarga.go).
func dropCache(path string) {}
