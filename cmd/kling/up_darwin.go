//go:build darwin

package main

import "syscall"

// versionMacOS lee la versión del sistema ("15.5") sin lanzar sw_vers.
func versionMacOS() string {
	v, err := syscall.Sysctl("kern.osproductversion")
	if err != nil {
		return ""
	}
	return v
}
