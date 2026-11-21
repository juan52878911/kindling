//go:build !linux

package net

import (
	"errors"
	"os/exec"
)

// ejecutarEnNetns: los netns de red del host solo existen en Linux (en macOS
// la red es de espacio de usuario dentro de kling-vz).
func ejecutarEnNetns(ns string, cmd *exec.Cmd) error {
	return errors.New("network namespaces are only available on Linux")
}
