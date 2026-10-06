package oci

import "os"

// duenoRoot: en Windows no hay root; nunca se confía sin rehashear.
func duenoRoot(os.FileInfo) bool { return false }
