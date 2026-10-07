package machine

// El entorno de una máquina dado al arrancarla (api.RunRequest.Env; el flujo
// entero, en pkg/api/machine_env.go).
//
// Lo que el daemon hace con los valores: validarlos, escribirlos en el
// almacén MMDS del VMM ANTES de InstanceStart y borrarlos de allí en cuanto
// el agente contesta. Nada más: no van a state.json (la máquina guarda los
// nombres, EnvKeys), ni a meta.json de un snapshot, ni a un log, ni al argv
// del VMM. Un error nombra la clave, nunca el valor.
//
// Las copias de un dorado no reciben entorno: la copia es la memoria del
// dorado, cuyo servicio ya arrancó con el suyo, y otro entorno no le llegaría
// (un POSTGRES_PASSWORD solo cuenta en el initdb). Heredan el del dorado —que
// está en esa memoria— y sus nombres. Pedir otro es un error, no un silencio.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	"github.com/juan52878911/kindling/pkg/api"
)

// ErrEnvRequest: el entorno pedido no vale, o se pidió con From (400).
var ErrEnvRequest = errors.New("invalid machine environment")

// plazoEntornoMMDS es cuánto se espera al agente antes de borrar el entorno
// de MMDS igualmente: un agente que no contestó en este tiempo tampoco lo va
// a leer ya (lo lee en sus primeros segundos, o no arranca el servicio).
const plazoEntornoMMDS = 2 * time.Minute

// entornoDePeticion valida req.Env. Con From, cualquier entorno es un error.
func entornoDePeticion(req api.RunRequest) (map[string]string, error) {
	if len(req.Env) == 0 {
		return nil, nil
	}
	if req.From != "" {
		return nil, fmt.Errorf("%w: a machine from a snapshot runs with the environment of the machine it was "+
			"saved from (its service is already running with it); boot one cold with -image to give it another", ErrEnvRequest)
	}
	env, err := api.MachineEnvMap(req.Env)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEnvRequest, err)
	}
	return env, nil
}

// envBootArg es el aviso al agente de que hay entorno en MMDS.
func envBootArg(hay bool) string {
	if !hay {
		return ""
	}
	return " " + api.MachineEnvBootParam + "=1"
}

// ponerEntornoMMDS deja env en el almacén MMDS de c. Va antes de Start: el
// almacén está vacío y el PUT no pisa nada.
func ponerEntornoMMDS(ctx context.Context, c *fc.Client, env map[string]string) error {
	if err := c.PutMMDSData(ctx, map[string]any{api.MachineEnvMMDSKey: env}); err != nil {
		return fmt.Errorf("giving the machine its environment via MMDS: %w", err)
	}
	return nil
}

// borrarEntornoMMDS quita el entorno del almacén (un null en un merge patch)
// sin tocar lo demás: marcadores de credenciales, secretos de sesión.
func borrarEntornoMMDS(ctx context.Context, c *fc.Client) error {
	return c.PatchMMDSData(ctx, map[string]any{api.MachineEnvMMDSKey: nil})
}

// retirarEntornoMMDS espera en segundo plano a que el agente de la máquina id
// conteste —para entonces ya leyó su entorno— y lo borra de MMDS: un proceso
// que se lance después dentro (un exec como otro usuario) ya no lo encuentra
// allí. Si el agente no anuncia que sabe leerlo (una imagen de antes), lo
// dice: su servicio arrancó sin ese entorno.
func (m *Manager) retirarEntornoMMDS(id, nombre string, env map[string]string) {
	m.mu.Lock()
	if m.entornoPendiente == nil {
		m.entornoPendiente = map[string]map[string]string{}
	}
	m.entornoPendiente[id] = env
	m.mu.Unlock()
	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.entornoPendiente, id)
			m.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), plazoEntornoMMDS)
		defer cancel()
		go func() {
			select {
			case <-m.quit:
				cancel()
			case <-ctx.Done():
			}
		}()
		ag := m.esperarAgente(ctx, id)
		m.mu.RLock()
		sock := m.socket[id]
		m.mu.RUnlock()
		if sock != "" {
			bctx, bcancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := borrarEntornoMMDS(bctx, fc.New(sock)); err != nil {
				log.Printf("warning: %s: could not remove its environment from MMDS: %v", nombre, err)
			}
			bcancel()
		}
		if ag != nil && !ag.Has(api.GuestCapEnv) {
			// Un servicio que esperaba su contraseña o su clave corriendo sin
			// ella es peor que no arrancar: la máquina falla (y -wait-ready con
			// ella) en vez de quedarse "lista" con el entorno equivocado.
			m.mu.RLock()
			mc := m.byID[id]
			m.mu.RUnlock()
			if mc != nil {
				m.fail(mc, errors.New("its guest agent does not read the machine environment (an image from "+
					"before kling run -e), so the service would run without it: rebuild or reimport the image"))
			}
		}
	}()
}

// esperarAgente pregunta al agente de id por su /healthz hasta que conteste
// o venza ctx. nil si no contestó o la máquina dejó de estar en marcha.
func (m *Manager) esperarAgente(ctx context.Context, id string) *api.GuestAgent {
	for ctx.Err() == nil {
		m.mu.RLock()
		mc := m.byID[id]
		var addr string
		if mc != nil && mc.State == api.StateRunning && mc.Reachable() {
			addr = mc.Addr(api.GuestPort)
		}
		m.mu.RUnlock()
		if addr == "" {
			return nil
		}
		// El plazo de /healthz, no el de /ready: no hay sonda detrás, y una
		// petición perdida (el agente aún no escuchaba) con 125 s se comía la
		// espera entera en vez de repetirse.
		pctx, pcancel := context.WithTimeout(ctx, plazoPeticionAgente)
		ag, err := preguntarAgente(pctx, "http://"+addr)
		pcancel()
		if err == nil {
			return ag
		}
		select {
		case <-ctx.Done():
		case <-time.After(pasoListo):
		}
	}
	return nil
}

// conEntornoPendiente añade a doc, el almacén MMDS que va a sustituir al
// actual (PutMMDS), el entorno de la máquina si su agente aún no lo ha leído:
// un `kling machine secret` en los primeros segundos se lo llevaba por delante
// y el servicio no arrancaba. Si doc no es un objeto no hay dónde ponerlo, y
// se pide repetir en unos segundos.
func (m *Manager) conEntornoPendiente(id string, doc any) (any, error) {
	m.mu.RLock()
	env, pendiente := m.entornoPendiente[id]
	m.mu.RUnlock()
	if !pendiente {
		return doc, nil
	}
	// Llega como JSON sin decodificar (el daemon no lo interpreta) o ya como
	// objeto (conMarcadores): se normaliza a objeto.
	obj, ok := doc.(map[string]any)
	if !ok {
		b, err := json.Marshal(doc)
		if err == nil && string(b) != "null" {
			err = json.Unmarshal(b, &obj)
		}
		if err != nil {
			return nil, errors.New("the machine is still reading its environment from MMDS; try again in a few seconds")
		}
	}
	out := make(map[string]any, len(obj)+1)
	for k, v := range obj {
		out[k] = v
	}
	out[api.MachineEnvMMDSKey] = env
	return out, nil
}
