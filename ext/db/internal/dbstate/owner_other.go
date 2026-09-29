//go:build !unix

package dbstate

import "io/fs"

// ownedByUs: sin uid que comparar fuera de unix.
func ownedByUs(fs.FileInfo) error { return nil }
