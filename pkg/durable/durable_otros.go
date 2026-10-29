//go:build !darwin

package durable

import "os"

// Ordenar lleva al disco lo escrito en f antes de lo que venga después. Fuera
// de macOS el fsync(2) ya llega al disco físico y no hay barrera más barata:
// es File.Sync (ver durable_darwin.go).
func Ordenar(f *os.File) error { return f.Sync() }
