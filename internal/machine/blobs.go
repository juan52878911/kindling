package machine

import (
	"fmt"

	"github.com/juan52878911/kindling/pkg/api"
)

// ImageReplaceable dice si se puede SUSTITUIR el contenido de una imagen (lo
// que hace un PUT /images/{name}/blob). Son las mismas dependencias que
// impiden borrarla: un dorado o una máquina viva que la tengan abierta, o
// capas que la usen de base, verían cambiar su rootfs por debajo. Quien llama
// solo pregunta si el contenido nuevo es DISTINTO: uno idéntico no cambia nada.
func (m *Manager) ImageReplaceable(name string) error {
	for _, otra := range m.Images() {
		if otra == name {
			continue
		}
		if b, ok := m.ImageBase(otra); ok && b == name {
			return fmt.Errorf("image %q is the base of %q: replacing it with different content would change that layer's rootfs",
				name, otra)
		}
	}
	for _, s := range m.Snapshots() {
		if s.Image == name {
			return fmt.Errorf("image %q is used by the golden snapshot %q and the new content is different: "+
				"remove that service first, or copy it under another name", name, s.Name)
		}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.arrancando[name] > 0 {
		return fmt.Errorf("image %q is being booted by a machine right now and the new content is different", name)
	}
	for _, mc := range m.byID {
		if mc.Image == name && mc.State != api.StateStopped && mc.State != api.StateFailed {
			return fmt.Errorf("image %q is in use by machine %q and the new content is different", name, mc.Name)
		}
	}
	return nil
}

// KernelReplaceable dice si se puede sustituir el vmlinux por otro distinto:
// no con máquinas que no estén paradas, porque arrancaron con el de ahora y
// sus snapshots lo llevan dentro de la memoria.
func (m *Manager) KernelReplaceable() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.arrancandoTotal > 0 {
		return fmt.Errorf("the kernel is being read by a machine that is booting and the new one is different: try again")
	}
	for _, mc := range m.byID {
		if mc.State != api.StateStopped && mc.State != api.StateFailed {
			return fmt.Errorf("the kernel is in use by machine %q and the new one is different: "+
				"remove or stop the machines first", mc.Name)
		}
	}
	return nil
}

// ConImagenesQuietas ejecuta fn sin ningún arranque en frío que empiece a
// leer una imagen mientras tanto. Es para sustituir una imagen o el kernel:
// ImageReplaceable (o KernelReplaceable) y el rename DENTRO de fn, o un run
// que llegue entre las dos arrancaría con el contenido que se comprobó que
// no se podía cambiar. fn no puede arrancar máquinas.
func (m *Manager) ConImagenesQuietas(fn func() error) error {
	m.imgMu.Lock()
	defer m.imgMu.Unlock()
	return fn()
}

// reservarImagen apunta que run va a arrancar en frío con image, hasta que la
// máquina esté en byID (donde ya la ve ImageReplaceable). Espera a que acabe
// una sustitución en curso. Devuelve con qué soltarla.
func (m *Manager) reservarImagen(image string) func() {
	m.imgMu.Lock()
	m.mu.Lock()
	if m.arrancando == nil {
		m.arrancando = map[string]int{}
	}
	m.arrancando[image]++
	m.arrancandoTotal++
	m.mu.Unlock()
	m.imgMu.Unlock()
	return func() {
		m.mu.Lock()
		if m.arrancando[image]--; m.arrancando[image] <= 0 {
			delete(m.arrancando, image)
		}
		m.arrancandoTotal--
		m.mu.Unlock()
	}
}
