package machine

import (
	"fmt"

	"github.com/juan52878911/kindling/internal/fc"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/config"
)

// Caudal de red por microVM. El limitador de Firecracker (RxLimiter y
// TxLimiter de eth0) evita que una máquina sature la red del host; con 16 MiB/s
// fijos, las carpetas compartidas (que viajan por esa red) se quedaban en eso.
// Ahora lo pide la máquina (RunRequest.NetMiBps), y si no, el daemon
// (daemon.net_mibps o KLING_NET_MIBPS). Se aplica en el arranque en frío: una
// copia de un dorado hereda el limitador que quedó en su snapshot.

// SetNetMiBps fija de dónde lee el manager daemon.net_mibps. Se consulta en
// cada arranque en frío: cambiarlo no pide reiniciar el daemon.
func (m *Manager) SetNetMiBps(f func() int) { m.netMiBps = f }

// validarNetMiBps rechaza un caudal pedido fuera de rango (0 = el del daemon).
func validarNetMiBps(n int) error {
	if n < 0 || n > config.MaxNetMiBps {
		return fmt.Errorf("net_mibps %d: the network limit goes from 1 to %d MiB/s (0 = the daemon's, daemon.net_mibps)", n, config.MaxNetMiBps)
	}
	return nil
}

// interfazRed es la eth0 que se le da a Firecracker en el arranque en frío.
// tap0 se llama igual en todos los namespaces: por eso el snapshot vale para
// cualquier instancia.
func (m *Manager) interfazRed(id string) fc.NetworkInterface {
	bps := m.caudalRedDe(id)
	return fc.NetworkInterface{
		IfaceID: "eth0", HostDevName: knet.TapName, GuestMAC: knet.GuestMAC,
		RxLimiter: fc.Limit(bps), TxLimiter: fc.Limit(bps),
	}
}

// caudalRedDe es el caudal (bytes/s, en cada sentido) para la máquina id.
func (m *Manager) caudalRedDe(id string) int64 {
	m.mu.RLock()
	var pedido int
	if mc := m.byID[id]; mc != nil {
		pedido = mc.NetMiBps
	}
	m.mu.RUnlock()
	return caudalRed(pedido, m.netMiBps)
}

// caudalRed elige: lo pedido, si no el del daemon, si no defaultNetMiBps. Un
// valor del daemon fuera de rango (un fichero editado a mano) cae al de
// siempre en vez de quitar el límite.
func caudalRed(pedido int, daemon func() int) int64 {
	n := pedido
	if n <= 0 && daemon != nil {
		n = daemon()
	}
	if n <= 0 || n > config.MaxNetMiBps {
		n = defaultNetMiBps
	}
	return int64(n) << 20
}
