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

// machineBoot es lo que el agente lee de MMDS al arrancar: el entorno y el
// comando de la máquina, cada uno con su error.
type machineBoot struct {
	env    map[string]string
	envErr error
	cmd    *api.MachineCmd
	cmdErr error
}

// loadMachineBoot lee de MMDS, con fetch (FetchMMDS fuera de las pruebas), lo
// que se pida: el entorno, el comando o los dos, con UNA tanda de reintentos
// para todo. Antes eran dos tandas seguidas, y con un MMDS mudo el agente
// tardaba el doble (~23 s) en contestar a /healthz. Un almacén sin la clave
// es un error: el host dijo que estaba. Un comando que no pasa
// api.CheckMachineCmd no se reintenta: no va a cambiar.
func loadMachineBoot(needEnv, needCmd bool, fetch func() (*MMDSStore, error)) machineBoot {
	var b machineBoot
	var envLast, cmdLast error
	envDone, cmdDone := !needEnv, !needCmd
	for i := 0; i < machineEnvTries && !(envDone && cmdDone); i++ {
		if i > 0 {
			time.Sleep(machineEnvPause)
		}
		st, err := fetch()
		if !envDone {
			switch {
			case err != nil:
				envLast = err
			case st == nil || st.MachineEnv == nil:
				envLast = errors.New("the MMDS store has no " + api.MachineEnvMMDSKey)
			default:
				b.env, envDone = st.MachineEnv, true
			}
		}
		if !cmdDone {
			switch {
			case err != nil:
				cmdLast = err
			case st == nil || st.MachineCmd == nil:
				cmdLast = errors.New("the MMDS store has no " + api.MachineCmdMMDSKey)
			default:
				if err := api.CheckMachineCmd(*st.MachineCmd); err != nil {
					b.cmdErr = err
				} else {
					b.cmd = st.MachineCmd
				}
				cmdDone = true
			}
		}
	}
	if !envDone {
		b.envErr = fmt.Errorf("reading the machine environment from MMDS: %w", envLast)
	}
	if !cmdDone {
		b.cmdErr = fmt.Errorf("reading the machine command from MMDS: %w", cmdLast)
	}
	return b
}

// loadMachineEnv lee solo el entorno de la máquina (ver loadMachineBoot).
func loadMachineEnv(fetch func() (*MMDSStore, error)) (map[string]string, error) {
	b := loadMachineBoot(true, false, fetch)
	return b.env, b.envErr
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

// applyMachineBoot lee de MMDS, de una vez, el entorno (con kling.env=1) y
// el comando (con kling.cmd=1) de la máquina. El entorno va a a.Env; si no se
// puede leer, se apunta en a.envErr y el servicio no arrancará (lo mismo con
// el comando y a.cmdErr). En la consola, solo los nombres.
func (a *Agent) applyMachineBoot(needEnv, needCmd bool, fetch func() (*MMDSStore, error)) {
	if !needEnv && !needCmd {
		return
	}
	b := loadMachineBoot(needEnv, needCmd, fetch)
	if needEnv {
		if b.envErr != nil {
			a.envErr = b.envErr
			log.Printf("machine env: %v; the service will not start without it", b.envErr)
		} else {
			a.Env = overlayEnv(a.Env, b.env)
			log.Printf("machine env: %d variable(s) from the host: %s", len(b.env), strings.Join(api.MachineEnvKeys(b.env), " "))
		}
	}
	if needCmd {
		if b.cmdErr != nil {
			a.cmdErr = b.cmdErr
			log.Printf("machine command: %v; the service will not start without it", b.cmdErr)
		} else {
			a.machineCmd = b.cmd
		}
	}
}

// applyMachineEnv es applyMachineBoot solo para el entorno.
func (a *Agent) applyMachineEnv(required bool, fetch func() (*MMDSStore, error)) {
	a.applyMachineBoot(required, false, fetch)
}
