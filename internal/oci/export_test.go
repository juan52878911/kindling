package oci

// SetUnpackHook pone unpackHook (y lo quita con nil).
func SetUnpackHook(f func(zstd bool) func()) { unpackHook = f }
