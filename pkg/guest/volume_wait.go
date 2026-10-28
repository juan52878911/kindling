package guest

// kling.volwait=1: los volúmenes los monta el host, y el servicio espera.
//
// Sirve para guardar una plantilla de un servicio con volúmenes (el pgdata de
// un postgres) sin congelar en su memoria la caché de un ext4 que después
// cambia. La máquina de preparación arranca con sus discos puestos pero sin
// montarlos, y el servicio sin lanzar: se congela así. Cada copia, al
// restaurarse, recibe POST /volume/acquire del daemon —como cualquier copia
// de un dorado con volúmenes— y en ese momento se monta y el servicio
// arranca, sobre los datos de ahora.
//
// No hay plazo: un servicio que arrancara sin su volumen escribiría en el
// overlay (un initdb que muere con la máquina) y eso es peor que esperar.

import (
	"log"

	"github.com/juan52878911/kindling/pkg/api"
)

// volumeWaitWanted dice si el host pidió kling.volwait=1.
func volumeWaitWanted() bool {
	return parseVolWait(cmdlineParams().values)
}

func parseVolWait(values map[string]string) bool {
	return values[api.VolumeWaitBootParam] == "1"
}

// acquiredCh es el canal que se cierra con el primer montaje que sale bien.
func (v *Volumes) acquiredCh() chan struct{} {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.acquired == nil {
		v.acquired = make(chan struct{})
	}
	return v.acquired
}

// noteAcquiredLocked avisa a quien espera. Con v.mu tomado.
func (v *Volumes) noteAcquiredLocked() {
	if v.acquired == nil {
		v.acquired = make(chan struct{})
	}
	select {
	case <-v.acquired:
	default:
		close(v.acquired)
	}
}

// startAfterVolumes lanza start cuando el host monte los volúmenes.
func startAfterVolumes(v *Volumes, start func()) {
	ch := v.acquiredCh()
	go func() {
		log.Printf("service: waiting for the host to mount the volumes (%s=1)", api.VolumeWaitBootParam)
		<-ch
		log.Printf("service: volumes mounted; starting it")
		start()
	}()
}
