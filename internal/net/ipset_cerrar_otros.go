//go:build !linux && !darwin

package net

// cerrarIPSet no tiene nada que cerrar fuera de Linux. Solo hace falta donde
// compila netns_fc.go (todo menos darwin); en darwin sobraría.
func cerrarIPSet(ns string) {}
