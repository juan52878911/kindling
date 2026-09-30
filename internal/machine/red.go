package machine

import (
	"log"
	"os"
	"path/filepath"
	"time"

	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/lazyre"
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
	// La marca va ANTES de montar: si el daemon muere a medias, lo que quede
	// montado también es suyo y su barrido lo puede recoger.
	m.marcarRedPropia(n.NS)
	// La reserva del índice (asignarRed, redParaRehacer) se suelta al acabar:
	// desde ahí la ocupa la dirección del veth (ver internal/net/subredes.go).
	defer n.SoltarReserva()
	if err := n.Setup(egress, domains, m.priv.UID); err != nil {
		return err
	}
	m.redMontada.Store(id, true)
	return nil
}

// redParaRehacer da la red con la que rehacer la de una máquina que ya tenía
// índice (thaw sin la red montada): la de su índice si sigue libre en el host,
// y si no, una nueva, que se apunta en la máquina.
//
// Su índice lo pudo tomar otro daemon del host mientras la red estaba
// desmontada: no lo ve, porque no hay dirección en el host que lo delate. El
// invitado no nota el cambio (su IP es siempre knet.GuestIP); lo que cambia es
// la IP por la que el host la alcanza (Machine.IP) y la del resolver, que el
// invitado pudo guardar en su caché de DNS.
func (m *Manager) redParaRehacer(mc *api.Machine) (*knet.Net, error) {
	n := knet.Plan(mc.NetIndex, mc.ID)
	if n.Reservar() {
		return n, nil
	}
	nueva, err := m.asignarRed(mc.ID)
	if err != nil {
		return nil, err
	}
	log.Printf("network: %s: its subnet (index %d, %s) is in use on this host, probably by another kindling daemon; "+
		"moving it to index %d (%s)", mc.Name, mc.NetIndex, n.HostIP, nueva.Index, nueva.HostIP)
	m.mu.Lock()
	if cur := m.byID[mc.ID]; cur != nil {
		cur.NetIndex, cur.IP = nueva.Index, nueva.NSIP
		m.persist()
	}
	m.mu.Unlock()
	mc.NetIndex, mc.IP = nueva.Index, nueva.NSIP
	return nueva, nil
}

// desmontarRed la desmonta y la olvida. Es la única forma de desmontar la red
// de una máquina: si se desmontara por otro camino, redLista la daría por buena.
func (m *Manager) desmontarRed(n *knet.Net, id string) {
	m.redMontada.Delete(id)
	n.Teardown()
	m.olvidarRedPropia(n.NS)
}

// DE QUIÉN ES CADA NAMESPACE.
//
// Los namespaces de red se llaman kl-<8 del id> y viven en el espacio global
// del host (ip netns), no bajo $KLING_ROOT. Dos daemons en el mismo host (el
// del sistema y uno privado con otro -root) veían los mismos, y el barrido de
// huérfanos de reconcile —"un kl-* que no está en MI estado"— borraba el
// namespace y el veth de una máquina VIVA del otro: se quedaba corriendo e
// incomunicada (medido en el lab, CT con Firecracker).
//
// Así que cada daemon apunta los suyos en <root>/net/<ns> al montarlos y solo
// barre lo apuntado. Lo que no lleva marca es de otro daemon o de un daemon
// anterior a esto: se deja y se dice una vez. Un huérfano sin marca de una
// versión vieja se queda (unos KiB de memoria del kernel); borrar la red de
// otro no tiene arreglo.

// nsValido: solo nombres que el propio daemon genera. Lo que lista `ip netns`
// acaba en una ruta bajo la raíz.
var nsValido = lazyre.New(`^kl-[0-9a-f]{1,16}$`)

func (m *Manager) dirRedPropia() string { return filepath.Join(m.root, "net") }

func (m *Manager) marcarRedPropia(ns string) {
	if m.root == "" || !nsValido.MatchString(ns) {
		return
	}
	dir := m.dirRedPropia()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("warning: could not record namespace %s as this daemon's: %v", ns, err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, ns), nil, 0o600); err != nil {
		log.Printf("warning: could not record namespace %s as this daemon's: %v", ns, err)
	}
}

func (m *Manager) olvidarRedPropia(ns string) {
	if m.root == "" || !nsValido.MatchString(ns) {
		return
	}
	_ = os.Remove(filepath.Join(m.dirRedPropia(), ns))
}

func (m *Manager) redEsPropia(ns string) bool {
	if m.root == "" || !nsValido.MatchString(ns) {
		return false
	}
	_, err := os.Stat(filepath.Join(m.dirRedPropia(), ns))
	return err == nil
}

// Sustituibles en tests: listar y borrar namespaces pide root e `ip`.
var (
	listarNamespaces = knet.ListNamespaces
	borrarNamespace  = knet.TeardownNamespace
)

// barrerNamespaces borra los namespaces huérfanos DE ESTE daemon: los que
// llevan su marca y no son de ninguna máquina suya (seen). Los demás no se
// tocan. Quita también las marcas de namespaces que ya no existen.
func (m *Manager) barrerNamespaces(seen map[string]bool) {
	existen := map[string]bool{}
	for _, ns := range listarNamespaces() {
		existen[ns] = true
		if seen[ns] {
			// Es de una máquina de este daemon. Si no lleva marca, la montó
			// un daemon anterior a las marcas (actualizar con máquinas vivas:
			// KillMode=process las deja correr y aquí se readoptan): se le
			// pone, para que si un día queda huérfano se pueda barrer.
			if !m.redEsPropia(ns) {
				m.marcarRedPropia(ns)
			}
			continue
		}
		if !m.redEsPropia(ns) {
			log.Printf("reconcile: leaving namespace %s alone: it was not created by this daemon (root %s); "+
				"another kindling daemon on this host may be using it", ns, m.root)
			continue
		}
		log.Printf("reconcile: cleaning up orphan namespace %s", ns)
		borrarNamespace(ns)
		m.olvidarRedPropia(ns)
	}
	ents, err := os.ReadDir(m.dirRedPropia())
	if err != nil {
		return
	}
	for _, e := range ents {
		if ns := e.Name(); !existen[ns] && !seen[ns] {
			m.olvidarRedPropia(ns)
		}
	}
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
