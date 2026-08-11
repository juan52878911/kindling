//go:build !linux

package main

import "errors"

// En el anfitrión (macOS, donde se desarrolla) no hay volumen que montar: el
// puente local expone un MCP de stdio y nada más. Estas versiones existen para
// que `go build ./...` compile fuera de Linux, no para usarse.

func mountVolume() (string, bool, error) {
	if mp, _ := volumeMountpoint(); mp != "" {
		return "", false, errors.New("los volúmenes solo existen dentro de la microVM")
	}
	return "", false, nil
}

func syncVolume(string)    {}
func unmountVolume(string) {}
