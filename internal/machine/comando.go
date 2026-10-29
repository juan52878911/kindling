package machine

// El comando de una máquina (api.RunRequest.Entrypoint/Cmd): el ENTRYPOINT y
// el CMD de la imagen cambiados para esta máquina sin hornear otra imagen.
//
// Viaja como el entorno (entorno.go): en el almacén MMDS antes de
// InstanceStart (api.MachineCmdMMDSKey), con kling.cmd=1 en la línea del
// kernel, y se borra de allí cuando el agente contesta. A diferencia del
// entorno no es secreto: la máquina lo guarda (Machine.Entrypoint/Cmd), se ve
// en `kling inspect` y un Start vuelve a dárselo.
//
// Solo en frío. Una copia de un dorado es su memoria, con el servicio ya
// lanzado (o, con ShareWait, a punto de lanzarse con lo que el agente leyó al
// arrancar el dorado): otro comando no le llegaría.

import (
	"errors"
	"fmt"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/guest"
)

// ErrCmdRequest: el comando pedido no vale, o se pidió con From (400).
var ErrCmdRequest = errors.New("invalid machine command")

// comandoDePeticion valida el comando de req. nil si no trae ninguno.
func comandoDePeticion(req api.RunRequest) (*api.MachineCmd, error) {
	if req.Entrypoint == nil && req.Cmd == nil {
		return nil, nil
	}
	if req.From != "" {
		return nil, fmt.Errorf("%w: a machine from a snapshot runs the command of the machine it was saved from "+
			"(its service already started with it); boot one cold with -image to give it another", ErrCmdRequest)
	}
	c := api.MachineCmd{Entrypoint: req.Entrypoint, Cmd: req.Cmd}
	if err := api.CheckMachineCmd(c); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCmdRequest, err)
	}
	return &c, nil
}

// comandoDe es el comando guardado en mc, o nil.
func comandoDe(mc *api.Machine) *api.MachineCmd {
	if mc == nil || (mc.Entrypoint == nil && mc.Cmd == nil) {
		return nil
	}
	return &api.MachineCmd{Entrypoint: mc.Entrypoint, Cmd: mc.Cmd}
}

// comandoDeID es el comando de la máquina id (boot no recibe la máquina).
func (m *Manager) comandoDeID(id string) *api.MachineCmd {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return comandoDe(m.byID[id])
}

// cmdBootArg es el aviso al agente de que hay comando en MMDS.
func cmdBootArg(c *api.MachineCmd) string {
	if c == nil {
		return ""
	}
	return " " + api.MachineCmdBootParam + "=1"
}

// datosArranqueMMDS es lo que boot deja en el almacén MMDS antes de Start:
// el entorno y el comando de la máquina, los que haya.
func datosArranqueMMDS(env map[string]string, c *api.MachineCmd) map[string]any {
	doc := map[string]any{}
	if len(env) > 0 {
		doc[api.MachineEnvMMDSKey] = env
	}
	if c != nil {
		doc[api.MachineCmdMMDSKey] = c
	}
	if len(doc) > 0 {
		doc[guest.MMDSVersionKey] = guest.MMDSVersion
	}
	return doc
}
