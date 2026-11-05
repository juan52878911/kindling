package machine

import (
	"os"
	"path/filepath"

	"github.com/juan52878911/kindling/pkg/api"
)

// Máquinas anidadas (RunRequest.Nested, docs/backend-hv.md): kling-hv crea la
// VM con EL2 y el invitado arranca con un kernel aparte que lleva KVM, así que
// puede correr Firecracker dentro. El resto de máquinas no cambia: siguen con
// images/vmlinux, cuyo sha256 firma los dorados.
//
// Una máquina anidada no se vuelca a disco: kling-hv no guarda el estado de
// EL2 en el snapshot y rechaza hacerlo. Freeze, commit y fork se niegan antes
// (con un error que dice por qué) y el TTL que congela se retira, como con
// HasSecrets.

// NestedKernelPath es el kernel con KVM de las máquinas anidadas
// (VARIANT=nested scripts/builders/kernel/build.sh).
func (m *Manager) NestedKernelPath() string { return filepath.Join(m.root, "images", "vmlinux-nested") }

// kernelDe es el kernel con que arranca una máquina.
func (m *Manager) kernelDe(anidada bool) string {
	if anidada {
		return m.NestedKernelPath()
	}
	return m.KernelPath()
}

// anidadaDe dice si la máquina id es anidada (lo lee boot, como
// esperaVolumenDe, para no alargar su firma).
func (m *Manager) anidadaDe(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mc := m.byID[id]
	return mc != nil && mc.Nested
}

// validarAnidada comprueba una petición con Nested antes de reservar nada. Si
// el Mac no da EL2, lo dice kling-hv al arrancar (InstanceStart), con su
// propio error.
func (m *Manager) validarAnidada(req api.RunRequest) error {
	if !req.Nested {
		return nil
	}
	if req.From != "" {
		return peticionInvalida("nested does not go with from: a nested machine has no snapshot to copy")
	}
	if b := m.Backend(); b != BackendHV {
		return peticionInvalida("nested virtualization needs the kling-hv backend (this daemon uses %s; docs/backend-hv.md)", b)
	}
	if _, err := os.Stat(m.NestedKernelPath()); err != nil {
		return peticionInvalida("nested machines need a kernel with KVM at %s: build it with "+
			"VARIANT=nested TARGET_ARCH=arm64 scripts/builders/kernel/build.sh (docs/backend-hv.md)", m.NestedKernelPath())
	}
	return nil
}

// errAnidadaSinVolcado es el porqué común de las negativas a volcar una
// máquina anidada a disco.
const errAnidadaSinVolcado = "it runs with nested virtualization, and kling-hv can't save the state of EL2 " +
	"(a snapshot would restore a broken KVM). Stop or remove it instead"
