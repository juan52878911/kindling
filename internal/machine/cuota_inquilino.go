package machine

// Cuotas por inquilino: cuánto puede ocupar a la vez lo que lleva
// kling.owner=<inquilino> (docs/authz.md). Las fija la política de
// autorización del daemon (SetCuotas); el manager solo cuenta y decide.
//
// Se deciden AQUÍ y no al revisar la petición en el daemon porque solo aquí
// hay un sitio en el que comprobar y apuntar son lo mismo: publicar mete la
// máquina en byID con m.mu tomado, y es ese mismo cerrojo el que cuenta lo
// que ya hay. Dos runs del mismo inquilino a la vez no pueden pasar los dos el
// tope: el segundo en tomar el cerrojo ya ve al primero. Una reserva en el
// daemon tendría que adivinar cuándo la máquina reservada aparece en byID, y
// mientras tanto la contaría dos veces (negando de más a quien lanza varias en
// paralelo).
//
// Para no hacer el trabajo caro (copiar el overlay, montar la red) de algo que
// ya no cabe, Run, runFrom, Fork y GraphUp miran antes la misma cuenta
// (comprobarCuota): es solo un filtro; la decisión es la de publicar.

import (
	"fmt"
	"os"

	"github.com/juan52878911/kindling/pkg/api"
)

// SinTope en un campo de Cuota: ese recurso no tiene límite.
const SinTope = -1

// Cuota son los topes de un inquilino. SinTope = sin límite; 0 = nada.
type Cuota struct {
	Maquinas int
	MemMiB   int
	DiscoMiB int
}

// UsoCuota es lo que ocupa algo a efectos de cuota.
type UsoCuota struct {
	Maquinas int
	MemMiB   int
	DiscoMiB int
}

func (u *UsoCuota) sumar(o UsoCuota) {
	u.Maquinas += o.Maquinas
	u.MemMiB += o.MemMiB
	u.DiscoMiB += o.DiscoMiB
}

// cuentaActiva dice si una máquina cuenta como máquina y como memoria: todas
// menos las paradas y las fallidas. Una congelada o pausada cuenta: vuelve a
// correr con un thaw, que no pasa por la cuota, y su memoria vuelve con ella.
func cuentaActiva(mc *api.Machine) bool {
	return mc.State != api.StateStopped && mc.State != api.StateFailed
}

// memCuota es la memoria que cuenta de una máquina: la mayor entre mem_mib y
// su techo mem_max_mib, porque resize la sube hasta ahí sin pasar por aquí.
func memCuota(mem, memMax int) int { return max(mem, memMax) }

// discoCuota es el tamaño LÓGICO de su disco escribible (0 = el de siempre):
// es lo que el invitado puede llegar a llenar, y lo único que se puede
// decidir al admitirla. Lo ocupado de verdad crece después, y para entonces
// ya no hay nada que negar.
func discoCuota(diskMiB int) int {
	if diskMiB > 0 {
		return diskMiB
	}
	return defaultOverlayMiB
}

// usoDe es lo que cuenta una máquina en el estado en que está. El disco
// cuenta en todos: existe hasta el rm, esté parada o no.
func usoDe(mc *api.Machine) UsoCuota {
	u := UsoCuota{DiscoMiB: discoCuota(mc.DiskMiB)}
	if cuentaActiva(mc) {
		u.Maquinas = 1
		u.MemMiB = memCuota(mc.MemMiB, mc.MemMaxMiB)
	}
	return u
}

// SetCuotas fija de dónde salen las cuotas: f da la de un inquilino, y false
// si no tiene. nil = sin cuotas, como siempre. Se llama antes de servir.
func (m *Manager) SetCuotas(f func(owner string) (Cuota, bool)) {
	if f == nil {
		m.cuotas.Store(nil)
		return
	}
	m.cuotas.Store(&f)
}

// cuotaDe es la cuota del inquilino owner, si tiene.
func (m *Manager) cuotaDe(owner string) (Cuota, bool) {
	if owner == "" {
		return Cuota{}, false
	}
	f := m.cuotas.Load()
	if f == nil {
		return Cuota{}, false
	}
	return (*f)(owner)
}

// cuotaExcedidaLocked dice si a owner le cabe pide además de lo que ya
// tiene en byID. excluir es una máquina que no se cuenta (la que Start va a
// arrancar: su uso nuevo va en pide). Se llama con m.mu tomado (basta de
// lectura).
func (m *Manager) cuotaExcedidaLocked(owner string, pide UsoCuota, excluir string) error {
	c, ok := m.cuotaDe(owner)
	if !ok {
		return nil
	}
	var lleva UsoCuota
	for id, mc := range m.byID {
		if id == excluir || mc.Labels[api.LabelOwner] != owner {
			continue
		}
		lleva.sumar(usoDe(mc))
	}
	return excedeCuota(owner, c, lleva, pide)
}

// excedeCuota compara. Solo se mira lo que pide sube: alguien por encima de
// un tope (porque se bajó) puede seguir haciendo lo que no lo aumenta.
func excedeCuota(owner string, c Cuota, lleva, pide UsoCuota) error {
	type recurso struct {
		campo           string
		tope, ya, extra int
		cuenta          string
	}
	for _, r := range []recurso{
		{"max_machines", c.Maquinas, lleva.Maquinas, pide.Maquinas, "it has %d machine(s) that aren't stopped or failed"},
		{"max_mem_mib", c.MemMiB, lleva.MemMiB, pide.MemMiB, "its machines that aren't stopped or failed take %d MiB"},
		{"max_disk_mib", c.DiscoMiB, lleva.DiscoMiB, pide.DiscoMiB, "the writable disks of its machines take %d MiB"},
	} {
		if r.tope == SinTope || r.extra <= 0 || r.ya+r.extra <= r.tope {
			continue
		}
		return &api.StatusError{Code: api.StatusTenantQuota, Message: fmt.Sprintf(
			"tenant %q %s: %s is %d and "+r.cuenta+"; this needs %d more.\n"+
				"Stop what it doesn't need (`kling stop`; only `kling rm` frees disk), "+
				"or ask an admin to raise its quota in the authz policy (docs/authz.md)",
			owner, api.TenantQuotaMark, r.campo, r.tope, r.ya, r.extra)}
	}
	return nil
}

// comprobarCuota es el filtro previo: ¿le cabe a owner pide con lo que ya
// tiene? No reserva nada; publicar decide.
func (m *Manager) comprobarCuota(owner string, pide UsoCuota) error {
	if _, ok := m.cuotaDe(owner); !ok {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cuotaExcedidaLocked(owner, pide, "")
}

// publicar mete una máquina recién creada en byID si le cabe a su dueño, en
// la misma sección crítica en que se cuenta (ver arriba).
func (m *Manager) publicar(mc *api.Machine) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.cuotaExcedidaLocked(mc.Labels[api.LabelOwner], usoDe(mc), ""); err != nil {
		return err
	}
	m.byID[mc.ID] = mc
	m.persist()
	return nil
}

// tamañoLogicoMiB es el tamaño lógico (redondeado hacia arriba) del disco
// escribible en ruta, con la convención de Machine.DiskMiB: 0 si es el de
// siempre o no se puede leer (y entonces cuenta como el de siempre).
func tamañoLogicoMiB(ruta string) int {
	fi, err := os.Stat(ruta)
	if err != nil {
		return 0
	}
	n := int((fi.Size() + 1<<20 - 1) >> 20)
	if n == defaultOverlayMiB {
		return 0
	}
	return n
}
