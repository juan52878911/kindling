package guest

// Parámetros kling.* de la línea de comandos del kernel.
//
// El agente se hornea en la imagen y el host se actualiza por su cuenta: un
// host nuevo arrancará imágenes con este agente durante años y les pasará
// parámetros que hoy no existen. La regla es que lo desconocido se IGNORA con
// un aviso en la consola, nunca se interpreta a medias ni tumba el arranque:
// el agente es PID 1 y, si muere, el kernel entra en pánico, que es un sitio
// pésimo para deducir que la imagen es más vieja que el host (docs/actualizar.md).
//
// Lo mismo dentro de un parámetro conocido: un modificador de volumen que no se
// entiende ("/data:ro:algo") no pasa a formar parte del nombre del directorio
// —como hacía el puente anterior a ":ro", que acababa en EACCES y pánico— sino
// que se descarta con un aviso.

import (
	"log"
	"os"
	"strings"
	"sync"

	"github.com/juan52878911/kindling/pkg/api"
)

// knownBootParams son los kling.* que este agente sabe leer. kling.layer lo lee
// overlay-init antes de que el agente exista, pero es del mismo contrato y no
// merece aviso.
var knownBootParams = map[string]bool{
	volumeBootParam:         true,
	execBootParam:           true,
	api.MachineEnvBootParam: true,
	"kling.layer":           true,
}

// bootParams es lo que se leyó de /proc/cmdline: los kling.* conocidos con su
// valor (el primero que aparece) y los desconocidos, para avisar de ellos.
type bootParams struct {
	values  map[string]string
	unknown []string
}

// parseBootParams parte la línea de comandos por palabras y se queda con los
// kling.*. No falla nunca: lo que no entiende va a unknown.
func parseBootParams(cmdline string) bootParams {
	p := bootParams{values: map[string]string{}}
	for _, tok := range strings.Fields(cmdline) {
		if !strings.HasPrefix(tok, "kling.") {
			continue
		}
		k, v, _ := strings.Cut(tok, "=")
		if !knownBootParams[k] {
			p.unknown = append(p.unknown, tok)
			continue
		}
		if _, dup := p.values[k]; !dup {
			p.values[k] = v
		}
	}
	return p
}

var (
	cmdlineOnce sync.Once
	cmdlineVal  bootParams
)

// cmdlineParams lee /proc/cmdline una vez por proceso (no cambia mientras el
// kernel vive) y avisa, también una vez, de los kling.* que no conoce. Sin
// /proc/cmdline (fuera de una microVM) no hay parámetros.
func cmdlineParams() bootParams {
	cmdlineOnce.Do(func() {
		b, err := os.ReadFile("/proc/cmdline")
		if err != nil {
			cmdlineVal = bootParams{values: map[string]string{}}
			return
		}
		cmdlineVal = parseBootParams(string(b))
		for _, tok := range cmdlineVal.unknown {
			log.Printf("warning: ignoring kernel parameter %s: this guest agent doesn't know it "+
				"(the host is newer than the image; rebuild it to use it)", tok)
		}
	})
	return cmdlineVal
}
