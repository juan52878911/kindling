//go:build !linux

package main

import "fmt"

// mountVolume fuera de Linux solo existe para que el paquete compile en la
// máquina donde se desarrolla. El puente SOLO corre dentro de la microVM.
func mountVolume() (string, error) {
	if volumeMountpoint() == "" {
		return "", nil
	}
	return "", fmt.Errorf("montar volúmenes solo funciona dentro de la microVM (Linux)")
}
