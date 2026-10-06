//go:build !linux

package main

// Fuera de Linux el daemon no baja de usuario a los constructores.
func limitarConstructor() error { return nil }
