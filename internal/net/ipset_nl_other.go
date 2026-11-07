//go:build !linux

package net

import "errors"

// Fuera de Linux no hay ipset: anadirIPSet cae al comando (que en macOS
// tampoco se usa; la red es de kling-vz).
func anadirIPSetNetlink(ns, set, ip string, timeout int) error {
	return errors.ErrUnsupported
}

func cerrarIPSet(ns string) {}
