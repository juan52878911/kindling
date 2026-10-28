package machine

// RunRequest.VolumeWait: la máquina arranca con sus volúmenes puestos y sin
// montar, y el servicio esperando a que el host se los monte (kling.volwait=1,
// pkg/guest/volume_wait.go).
//
// Es lo que deja guardar una plantilla de un postgres con su pgdata: un
// commit normal suelta los volúmenes antes de volcar la memoria y los
// devuelve después, y con el servicio corriendo encima no se pueden soltar.
// Así el commit no tiene nada que soltar —ni que devolver: devolverlos
// lanzaría el servicio en la máquina de preparación— y cada copia, al
// restaurarse, los monta (acquireVolumes, como cualquier dorado con
// volúmenes) y su agente lanza el servicio sobre los datos de ahora.

import (
	"context"
	"errors"
	"fmt"

	"github.com/juan52878911/kindling/pkg/api"
)

// validarVolumeWait comprueba que req.VolumeWait tenga sentido.
func validarVolumeWait(req api.RunRequest) error {
	if !req.VolumeWait {
		return nil
	}
	if req.From != "" {
		return fmt.Errorf("%w: volume_wait is for a cold boot; a copy of a snapshot mounts its volumes on restore", ErrShareRequest)
	}
	if len(req.VolumeSet()) == 0 {
		return fmt.Errorf("%w: volume_wait needs volumes to wait for", ErrShareRequest)
	}
	return nil
}

// esperaVolumenDe dice si la máquina id arranca con kling.volwait=1.
func (m *Manager) esperaVolumenDe(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mc := m.byID[id]
	return mc != nil && mc.VolumeWait
}

// volWaitBootArg es el aviso al agente.
func volWaitBootArg(espera bool) string {
	if !espera {
		return ""
	}
	return " " + api.VolumeWaitBootParam + "=1"
}

// vigilarEsperaVolumen da la máquina por fallida si su agente no entiende
// kling.volwait: habrá montado los volúmenes y lanzado el servicio, y el
// commit que viene detrás no podría soltarlos.
func (m *Manager) vigilarEsperaVolumen(id string) {
	go func() {
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
		if ag == nil || ag.Has(api.GuestCapVolumeWait) {
			return
		}
		m.mu.RLock()
		mc := m.byID[id]
		m.mu.RUnlock()
		if mc != nil {
			m.fail(mc, errors.New("its guest agent does not wait for its volumes (an image from before volume_wait), "+
				"so it mounted them and started the service: reimport the image"))
		}
	}()
}
