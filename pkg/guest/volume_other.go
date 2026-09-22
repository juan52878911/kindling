//go:build !linux

package guest

import "errors"

// En el anfitrión (macOS, donde se desarrolla) no hay volúmenes que montar: el
// puente local expone un MCP de stdio y nada más. Estas versiones existen para
// que `go build ./...` compile fuera de Linux, no para usarse.

func mountVolumes() ([]VolumeSpec, error) {
	if len(volumeSpecsFromCmdline()) > 0 {
		return nil, errors.New("volumes only exist inside the microVM")
	}
	return nil, nil
}

func syncVolumes([]VolumeSpec)    {}
func unmountVolumes([]VolumeSpec) {}
