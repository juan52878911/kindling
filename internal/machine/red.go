package machine

import (
	"path/filepath"
	"time"

	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

// LA RED SOBREVIVE AL FREEZE.
//
// Montar la red de una microVM son una docena de ip/iptables, cada uno un
// proceso (y los de dentro del namespace, dos: `ip netns exec` y el comando):
// ~47 ms medidos en un i7-8700T, un tercio de todo el thaw. Antes Freeze la
// desmontaba y Thaw la volvía a montar idéntica —mismo índice, misma IP, mismo
// tap0—, así que ahora se queda montada mientras la máquina duerme y Thaw solo
// comprueba que sigue ahí. De paso el tap0 es el MISMO dispositivo, con la
// misma MAC, y la caché ARP que el invitado congeló en su memoria sigue siendo
// buena.
//
// Lo que cuesta: un namespace con su veth, su tap y sus reglas por máquina
// congelada (kilobytes de memoria del kernel, nada de CPU), y, si la máquina es
// pequeña, su mem.file en la caché de página (reclamable; ver
// cacheTibiaMaxBytes). Para que no se acumulen, el vigilante suelta las dos
// cosas en las que llevan más de redDormidaMax congeladas
// (soltarRedesDormidas), y un reinicio del daemon desmonta la red de todas
// (reconcile): Thaw la rehace entonces como siempre.

// redDormidaMax es cuánto se conserva la red de una máquina congelada.
const redDormidaMax = 30 * time.Minute

// montarRed monta la red de la máquina id y apunta que está en pie.
func (m *Manager) montarRed(n *knet.Net, id string, egress knet.Egress, domains []string) error {
	m.redMontada.Delete(id)
	if err := n.Setup(egress, domains, m.priv.UID); err != nil {
		return err
	}
	m.redMontada.Store(id, true)
	return nil
}

// desmontarRed la desmonta y la olvida. Es la única forma de desmontar la red
// de una máquina: si se desmontara por otro camino, redLista la daría por buena.
func (m *Manager) desmontarRed(n *knet.Net, id string) {
	m.redMontada.Delete(id)
	n.Teardown()
}

// redLista dice si la red de la máquina id la montó este daemon y sigue en pie:
// su namespace y el veth del lado del host existen.
func (m *Manager) redLista(n *knet.Net, id string) bool {
	if _, ok := m.redMontada.Load(id); !ok {
		return false
	}
	if !n.Exists() {
		m.redMontada.Delete(id)
		return false
	}
	return true
}

// soltarRedesDormidas desmonta la red de las máquinas congeladas hace más de
// redDormidaMax. Con el candado de cada máquina, y sin esperarlo: una que se
// está descongelando ahora mismo no se toca.
func (m *Manager) soltarRedesDormidas() {
	type cand struct {
		id    string
		index int
	}
	var cands []cand
	m.mu.RLock()
	for id, mc := range m.byID {
		if mc.State != api.StateWarm || mc.FrozenAt == nil || time.Since(*mc.FrozenAt) < redDormidaMax {
			continue
		}
		if _, ok := m.redMontada.Load(id); ok {
			cands = append(cands, cand{id, mc.NetIndex})
		}
	}
	m.mu.RUnlock()
	for _, c := range cands {
		unlock, ok := m.tryLock(c.id)
		if !ok {
			continue
		}
		if mc, ok := m.get(c.id); ok && mc.State == api.StateWarm {
			m.desmontarRed(knet.Plan(c.index, c.id), c.id)
			// Y su memoria, que Freeze dejó en la caché si era pequeña.
			dropCache(filepath.Join(m.dir(c.id), "mem.file"))
		}
		unlock()
	}
}
