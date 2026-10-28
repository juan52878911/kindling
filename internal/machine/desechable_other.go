//go:build !darwin

package machine

import "time"

// arranqueHost solo hace falta con kling-vz (overlayDesechable): en Linux no
// hay overlays sin sincronizar.
func arranqueHost() (time.Time, bool) { return time.Time{}, false }
