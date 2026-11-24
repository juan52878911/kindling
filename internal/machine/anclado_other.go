//go:build !linux

package machine

// anclarFicheroPlataforma es nil fuera de Linux: en macOS el mem.file lo
// mapea kling-hv/kling-vz y el cuello es otro (plan de la ronda 9). El
// daemon responde 501 (ErrAncladoNoSoportado).
var anclarFicheroPlataforma func(path string) (anclaje, error)
