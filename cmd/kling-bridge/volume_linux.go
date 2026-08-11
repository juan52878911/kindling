//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
)

// mountVolume monta el volumen si el kernel dice que hay uno.
//
// Devuelve el punto de montaje, o "" si no había volumen que montar — que es el
// caso normal y no un error.
func mountVolume() (string, error) {
	mp := volumeMountpoint()
	if mp == "" {
		return "", nil
	}
	if _, err := os.Stat(volumeDevice); err != nil {
		return "", fmt.Errorf("el kernel pide montar %s en %s pero %s no existe: %w",
			volumeBootParam, mp, volumeDevice, err)
	}
	if err := os.MkdirAll(mp, 0o755); err != nil {
		return "", fmt.Errorf("creando %s: %w", mp, err)
	}
	// Sin flags de solo lectura: el sentido de un volumen es que lo que escriba
	// la herramienta sobreviva.
	if err := syscall.Mount(volumeDevice, mp, "ext4", 0, ""); err != nil {
		return "", fmt.Errorf("montando %s en %s: %w", volumeDevice, mp, err)
	}
	return mp, nil
}
