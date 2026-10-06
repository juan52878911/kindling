package guest

// El entorno de la máquina (api.RunRequest.Env, ver pkg/api/machine_env.go).
//
// El daemon lo deja en MMDS antes de arrancar y pone kling.env=1 en la línea
// del kernel. El agente lo lee UNA vez, al arrancar y antes de escuchar (el
// daemon lo borra del almacén en cuanto el agente contesta), y lo guarda solo
// en su memoria: ningún fichero del invitado lo lleva. Va encima del entorno
// de la imagen —el de /etc/kling/env que cargó el /entrypoint—: con la misma
// clave gana la máquina.
//
// Si kling.env=1 y no se puede leer, el servicio NO arranca: un servicio que
// arranca sin la contraseña o sin la opción de seguridad que se le pasó es
// peor que uno parado con un error en `kling logs`.

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// MachineEnvRequired dice si el host arrancó la máquina con entorno propio.
func MachineEnvRequired() bool {
	return cmdlineParams().values[api.MachineEnvBootParam] == "1"
}

// machineEnvTries y machineEnvPause: MMDS es del VMM y está desde el primer
// instante, pero la ruta a 169.254.169.254 se acaba de poner; unos reintentos
// cortos cubren un primer paquete perdido sin retrasar un arranque sano.
var (
	machineEnvTries = 5
	machineEnvPause = 400 * time.Millisecond
)

// loadMachineEnv lee el entorno de la máquina con fetch (FetchMMDS fuera de
// las pruebas). Un almacén sin la clave es un error: el host dijo que estaba.
func loadMachineEnv(fetch func() (*MMDSStore, error)) (map[string]string, error) {
	var last error
	for i := 0; i < machineEnvTries; i++ {
		if i > 0 {
			time.Sleep(machineEnvPause)
		}
		st, err := fetch()
		switch {
		case err != nil:
			last = err
		case st == nil || st.MachineEnv == nil:
			last = errors.New("the MMDS store has no " + api.MachineEnvMMDSKey)
		default:
			return st.MachineEnv, nil
		}
	}
	return nil, fmt.Errorf("reading the machine environment from MMDS: %w", last)
}

// overlayEnv pone m encima de base: una clave que ya estaba se queda en su
// sitio con el valor de m; las nuevas van al final, ordenadas.
func overlayEnv(base []string, m map[string]string) []string {
	out := make([]string, 0, len(base)+len(m))
	seen := make(map[string]bool, len(m))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if v, ok := m[k]; ok {
			if !seen[k] {
				out = append(out, k+"="+v)
				seen[k] = true
			}
			continue
		}
		out = append(out, kv)
	}
	var rest []string
	for k := range m {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		out = append(out, k+"="+m[k])
	}
	return out
}

// applyMachineEnv, con kling.env=1, lee el entorno de la máquina y lo pone en
// a.Env. Si no puede, lo apunta en a.envErr y el servicio no arrancará. En la
// consola, solo los nombres.
func (a *Agent) applyMachineEnv(required bool, fetch func() (*MMDSStore, error)) {
	if !required {
		return
	}
	m, err := loadMachineEnv(fetch)
	if err != nil {
		a.envErr = err
		log.Printf("machine env: %v; the service will not start without it", err)
		return
	}
	a.Env = overlayEnv(a.Env, m)
	log.Printf("machine env: %d variable(s) from the host: %s", len(m), strings.Join(api.MachineEnvKeys(m), " "))
}
