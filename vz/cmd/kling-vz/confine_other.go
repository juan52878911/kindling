//go:build !darwin

package main

// confinar fuera de macOS no hace nada: kling-vz solo corre en macOS.
func confinar(root, mdir string, conRed bool) error { return nil }
