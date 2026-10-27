package guest

// Los ulimits y sysctls del servicio (api.ServiceSpec.Ulimits y Sysctls): se
// aplican una vez, antes de la primera vez que se lanza.
//
// Los sysctls primero (fs.nr_open pone el techo de nofile). Los ulimits se
// ponen en el propio agente y no en el hijo: Go restaura el nofile original
// en cada hijo salvo que el proceso haya llamado a Setrlimit, y poner el
// límite después de lanzarlo llegaría tarde para un servicio que lo lee al
// arrancar. Así los hereda el servicio y también lo que lance /exec o la
// consola, como `docker exec` hereda los del contenedor.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
)

// procSysRoot es /proc/sys (pruebas).
var procSysRoot = "/proc/sys"

// applySysctls escribe cada sysctl en /proc/sys. Devuelve un aviso por cada
// uno que no se pudo: el servicio arranca igual y, si de verdad lo necesita,
// dirá por qué en su propia salida.
func applySysctls(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var warn []string
	for _, k := range keys {
		// La clave ya pasó api.CheckSysctl: sin "/" ni "..", solo puntos.
		p := filepath.Join(procSysRoot, strings.ReplaceAll(k, ".", "/"))
		if err := os.WriteFile(p, []byte(m[k]+"\n"), 0o644); err != nil {
			warn = append(warn, fmt.Sprintf("sysctl %s=%s: %v", k, m[k], err))
		}
	}
	return warn
}

// applyLimits aplica los sysctls y los ulimits del spec y devuelve los avisos.
func applyLimits(spec api.ServiceSpec) []string {
	var warn []string
	warn = append(warn, applySysctls(spec.Sysctls)...)
	us, err := api.ParseUlimits(spec.Ulimits)
	if err != nil {
		return append(warn, err.Error())
	}
	for _, u := range us {
		if err := setRlimit(u); err != nil {
			warn = append(warn, fmt.Sprintf("ulimit %s: %v", u.Name, err))
		}
	}
	return warn
}
