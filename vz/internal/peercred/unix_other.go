//go:build !darwin

package peercred

import "net"

// SameUser fuera de macOS no filtra: kling-vz y kling-hv solo corren en
// macOS, y esto existe para que el módulo compile y se pruebe en Linux.
func SameUser(l net.Listener, _ func(string, ...any)) net.Listener { return l }
