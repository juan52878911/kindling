package guest

// El comando de la máquina (api.RunRequest.Entrypoint/Cmd, ver
// internal/machine/comando.go).
//
// Como el entorno (machine_env.go): con kling.cmd=1 el agente lo lee de MMDS
// una vez, al arrancar y antes de escuchar, y sustituye con él el Argv de
// /etc/kindling/service.json. Si no puede leerlo o no puede aplicarlo, el
// servicio NO arranca: correr el comando de la imagen en vez del pedido
// parecería sano y no lo es.

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// MachineCmdRequired dice si el host arrancó la máquina con comando propio.
func MachineCmdRequired() bool {
	return cmdlineParams().values[api.MachineCmdBootParam] == "1"
}

// loadMachineCmd lee el comando de la máquina con fetch, con los mismos
// reintentos que el entorno.
func loadMachineCmd(fetch func() (*MMDSStore, error)) (*api.MachineCmd, error) {
	var last error
	for i := 0; i < machineEnvTries; i++ {
		if i > 0 {
			time.Sleep(machineEnvPause)
		}
		st, err := fetch()
		switch {
		case err != nil:
			last = err
		case st == nil || st.MachineCmd == nil:
			last = errors.New("the MMDS store has no " + api.MachineCmdMMDSKey)
		default:
			if err := api.CheckMachineCmd(*st.MachineCmd); err != nil {
				return nil, err
			}
			return st.MachineCmd, nil
		}
	}
	return nil, fmt.Errorf("reading the machine command from MMDS: %w", last)
}

// applyMachineCmd, con kling.cmd=1, lee el comando de la máquina y lo guarda
// para StartService. Si no puede, el servicio no arrancará.
func (a *Agent) applyMachineCmd(required bool, fetch func() (*MMDSStore, error)) {
	if !required {
		return
	}
	c, err := loadMachineCmd(fetch)
	if err != nil {
		a.cmdErr = err
		log.Printf("machine command: %v; the service will not start without it", err)
		return
	}
	a.machineCmd = c
}

// machineArgv es el Argv del servicio con el comando de la máquina encima
// del de la imagen, con las reglas de Docker: Cmd cambia los argumentos y
// conserva el ENTRYPOINT; Entrypoint cambia el ejecutable y, sin Cmd, deja
// los argumentos vacíos. spec nil es una imagen sin servicio.
func machineArgv(spec *api.ServiceSpec, c *api.MachineCmd) ([]string, error) {
	var entry, cmd []string
	if spec != nil {
		if spec.Init {
			return nil, errors.New("the image runs its own init as PID 1 (-init image): a machine command cannot replace it")
		}
		entry, cmd = spec.Entrypoint, spec.Cmd
		if entry == nil && cmd == nil && len(spec.Argv) > 0 && c.Entrypoint == nil {
			// Una imagen de antes: su Argv no dice dónde acaba el ENTRYPOINT.
			return nil, errors.New("the image's service does not say which part of its command is the ENTRYPOINT " +
				"(an image from before machine commands): reimport it")
		}
	}
	if c.Entrypoint != nil {
		entry, cmd = c.Entrypoint, nil
	}
	if c.Cmd != nil {
		cmd = c.Cmd
	}
	argv := append(append([]string{}, entry...), cmd...)
	if len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("the machine command leaves the service with nothing to run")
	}
	return argv, nil
}

// conMachineCmd aplica el comando de la máquina a svc (nil: la imagen no
// declara servicio, y el comando pasa a serlo). Devuelve el servicio a lanzar
// o el error por el que no se lanza.
func (a *Agent) conMachineCmd(svc *Service) (*Service, error) {
	if a.cmdErr != nil {
		return svc, a.cmdErr
	}
	if a.machineCmd == nil {
		return svc, nil
	}
	var spec *api.ServiceSpec
	if svc != nil {
		spec = &svc.spec
	}
	argv, err := machineArgv(spec, a.machineCmd)
	if err != nil {
		return svc, fmt.Errorf("machine command: %w", err)
	}
	if svc == nil {
		svc = newService(api.ServiceSpec{Restart: api.RestartOnFailure})
	}
	svc.spec.Argv = argv
	log.Printf("machine command: %s", strings.Join(argv, " "))
	return svc, nil
}
