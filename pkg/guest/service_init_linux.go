//go:build linux

package guest

import "syscall"

// syncFS vacía las cachés de disco: el init de la imagen puede llevarse al
// agente por delante antes de que el daemon pida vaciar los volúmenes.
func syncFS() { syscall.Sync() }
