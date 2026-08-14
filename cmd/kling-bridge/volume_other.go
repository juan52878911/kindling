//go:build !linux

package main

import "errors"

// En el anfitrión (macOS, donde se desarrolla) no hay volúmenes que montar: el
// puente local expone un MCP de stdio y nada más. Estas versiones existen para
// que `go build ./...` compile fuera de Linux, no para usarse.

func mountVolumes() ([]volumeSpec, error) {
	if len(volumeSpecs()) > 0 {
		return nil, errors.New("volumes only exist inside the microVM")
	}
	return nil, nil
}

func syncVolumes([]volumeSpec)    {}
func unmountVolumes([]volumeSpec) {}
