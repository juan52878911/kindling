//go:build darwin

package daemon

import (
	"log"
	"os/exec"
	"strings"

	"github.com/juan52878911/kindling/internal/machine"
)

// En macOS el daemon corre como el usuario: el socket ya es suyo.
const cederSocket = false

// Construir imágenes necesita root, loop y chroot de Linux: aquí se copian
// (`kling images copy`), no se construyen.
const construirImagenes = false

// comprobarHost avisa de lo que falta para arrancar microVMs en macOS: el
// ayudante kling-vz y e2fsprogs. No hay red del host que montar: cada
// ayudante lleva la suya.
func (s *Server) comprobarHost() {
	var missing []string
	if _, err := exec.LookPath(s.fcBin); err != nil {
		missing = append(missing, s.fcBin+" (build it with `make vz`, or set KLING_VMM to its path)")
	}
	if !machine.E2fsDisponible("mkfs.ext4") {
		missing = append(missing, "mkfs.ext4 (brew install e2fsprogs)")
	}
	if len(missing) > 0 {
		log.Printf("WARNING: missing binaries: %s: microVMs cannot be started on this host",
			strings.Join(missing, ", "))
	}
	// debugfs solo inspecciona imágenes: sin él se arranca igual, pero no se
	// sabe si una imagen lleva agente o si su base entiende capas, e `images
	// cat` no funciona. Decir que no se puede arrancar sería mentir.
	if !machine.E2fsDisponible("debugfs") {
		log.Printf("WARNING: debugfs not found (brew install e2fsprogs): microVMs run, " +
			"but images can't be inspected (images cat, agent and layer checks are skipped)")
	}
}
