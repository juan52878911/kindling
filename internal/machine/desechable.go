package machine

// Overlay desechable (kling_disposable, docs/backend-vz.md).
//
// kling-vz puede llevar el overlay de una máquina sin sincronizar los flush del
// invitado con el disco físico (synchronizationMode none): 300 sync en el
// invitado pasan de 1814 a 676 ms. El precio es que, si el Mac se cae, lo que
// el invitado creía en disco puede no estarlo, y el ext4 del overlay queda
// roto. Eso solo es aceptable para un disco que se iba a tirar igual.
//
// QUÉ ES DESECHABLE. Solo el overlay (vdb, la capa escribible propia) de una
// máquina con on_ttl=remove: los sandboxes (kling try, kling sandbox create,
// las plantillas del frontal mientras se construyen), que por contrato
// desaparecen al vencer su TTL aunque estén congelados. Nunca la base ni la
// capa (son de solo lectura), nunca los volúmenes ni los discos de kling db
// (son lo que sobrevive a la máquina), y nunca el overlay de una máquina con
// on_ttl=freeze: los servicios y las réplicas de un dorado se congelan y se
// despiertan durante días, y su overlay es parte de su estado.
//
// LO QUE SE CONSERVA SIGUE SIENDO DURABLE. Hasta una máquina con
// on_ttl=remove puede congelarse (kling freeze, el planificador) o pararse, y
// entonces su overlay se guarda para un thaw o un start. Cada vez que su VMM
// muere conservando el disco hay un fsync del overlay desde el host ANTES de
// que el estado lo anuncie: Freeze ya lo hacía (sincronizarVolcado, antes del
// sello), y Stop y fail lo hacen con asentarOverlay. Un corte de luz después
// no rompe nada que se vaya a reanudar. Lo único que queda expuesto es una
// máquina que corría cuando se fue la luz, y reconcile la marca fallida en
// vez de parada (ver fallidaTrasCorte): nadie la arranca sobre un ext4 a
// medias, y gcFailed la recoge. Un thaw no vuelve a marcar el overlay (el
// snapshot no guarda la marca, y la VM nace al cargar): tras despertar va
// sincronizado, que es lo seguro.
//
// Solo en kling-vz: Firecracker rechaza los campos que no conoce, así que con
// él el campo no viaja nunca (fc.Drive lo omite si es false).

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// overlayDesechable dice si el overlay de mc puede ir sin sincronizar.
func overlayDesechable(mc *api.Machine) bool {
	return vmmAdmiteDesechable(backendVMM) && mc != nil && mc.OnTTL == api.OnTTLRemove
}

// vmmAdmiteDesechable: solo kling-vz conoce kling_disposable.
func vmmAdmiteDesechable(backend string) bool { return backend == BackendVZ }

// overlayDesechableDe es overlayDesechable de la máquina registrada con id.
// boot la consulta así porque Run y Start la publican en byID antes de
// arrancarla; una que no esté registrada (tests, una abandonada) no lo es.
func (m *Manager) overlayDesechableDe(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return overlayDesechable(m.byID[id])
}

// asentarOverlay lleva al disco físico el overlay de mc si iba sin
// sincronizar. Se llama con el VMM ya muerto (nadie más escribe) y antes de
// anunciar un estado que conserva el disco. En macOS File.Sync es
// F_FULLFSYNC, que es lo que el modo none dejó de hacer. Freeze no lo
// necesita: sincronizarVolcado ya asienta el overlay antes del sello.
func (m *Manager) asentarOverlay(mc *api.Machine) error {
	if !overlayDesechable(mc) {
		return nil
	}
	// Sin seguir enlaces (el overlay está en un directorio del VMM), como
	// sincronizarVolcado.
	err := sincronizarFichero(m.overlayParaLeer(mc.ID), syscall.O_NOFOLLOW)
	if errors.Is(err, syscall.ELOOP) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("syncing the disposable overlay: %w", err)
	}
	return nil
}

// fallidaTrasCorte dice si una máquina que reconcile encuentra sin proceso,
// running o pausada y sin snapshot propio, debe quedar fallida y no parada: su
// overlay iba sin sincronizar, nadie lo asentó y el host se reinició desde que
// arrancó (arranque, de arranqueHost), así que puede estar a medias. Sin
// reinicio del host (el daemon se reinició, el VMM murió) lo que el invitado
// escribió sigue en la caché del host o ya en disco: parada, como siempre. Si
// no se sabe cuándo arrancó el host, se asume lo peor.
func fallidaTrasCorte(mc *api.Machine, arranque time.Time, conArranque bool) bool {
	if !overlayDesechable(mc) {
		return false
	}
	if !conArranque || mc.StartedAt == nil {
		return true
	}
	return arranque.After(*mc.StartedAt)
}

// errOverlaySinAsentar es el LastErr de esas máquinas.
const errOverlaySinAsentar = "the microVM stopped without a clean shutdown and its disposable overlay " +
	"(on_ttl=remove, not synced to disk) may be incomplete; remove it (kling rm)"
