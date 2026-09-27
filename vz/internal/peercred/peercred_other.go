//go:build !darwin

package peercred

import "net"

// Checker fuera de macOS no comprueba nada: kling-vz solo corre en macOS, y
// esto existe para que el módulo compile y se pruebe en Linux.
type Checker struct{}

func New() *Checker { return &Checker{} }

func (*Checker) Allowed(net.Conn) bool { return true }
