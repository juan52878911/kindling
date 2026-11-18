package machine

import (
	"context"
	"fmt"
	"log"
	"runtime"

	"github.com/juan52878911/kindling/internal/fc"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/config"
)

// Caudal de red por microVM. El limitador de Firecracker (RxLimiter y
// TxLimiter de eth0) evita que una máquina sature la red del host; con 16 MiB/s
// fijos, las carpetas compartidas (que viajan por esa red) se quedaban en eso.
// Ahora lo pide la máquina (RunRequest.NetMiBps), y si no, el daemon
// (daemon.net_mibps o KLING_NET_MIBPS). Se aplica en el arranque en frío; una
// copia de un dorado trae el limitador que quedó en su snapshot, y se le cambia
// tras cargarlo si tiene carpetas o pide otro caudal (caudalTrasCargar).

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
	bps, _ := m.caudalRedDe(id)
	return fc.NetworkInterface{
		IfaceID: "eth0", HostDevName: knet.TapName, GuestMAC: knet.GuestMAC,
		RxLimiter: fc.LimitOps(bps, opsRed(bps)), TxLimiter: fc.LimitOps(bps, opsRed(bps)),
	}
}

// limitadorEnVMM: el VMM aplica el limitador de eth0. Firecracker sí; kling-vz
// y kling-hv (macOS) no lo usan ni admiten cambiarlo. Variable para los tests.
var limitadorEnVMM = runtime.GOOS != "darwin"

// caudalTrasCargar le pone a una copia recién restaurada (aún en pausa) el
// caudal que le toca, si no es por fuerza el de su snapshot: el dorado arrancó
// en frío sin carpetas (16 MiB/s), y la copia, que viaja a sus carpetas por la
// red, se quedaba en eso. Es el camino habitual por el API (run -from -share,
// sandbox). Un fallo no tumba la copia: sigue con el limitador del snapshot.
func (m *Manager) caudalTrasCargar(ctx context.Context, c *fc.Client, id string) {
	if !limitadorEnVMM {
		return
	}
	bps, propio := m.caudalRedDe(id)
	if !propio {
		return
	}
	l := fc.LimitOps(bps, opsRed(bps))
	if err := c.PatchNetworkLimiters(ctx, "eth0", l, l); err != nil {
		log.Printf("warning: %s: could not set its network limit to %d MiB/s after restoring it; it keeps its snapshot's: %v", shortID(id), bps>>20, err)
	}
}

// bytesPorTramaMin es el tamaño medio de trama que el tope de tramas por
// segundo supone: con el caudal por defecto (16 MiB/s), 32768 tramas/s. Una
// transferencia normal va en tramas de 1,5 KiB o más (con TSO, hasta 64 KiB)
// y no lo toca; una inundación de paquetes de 64 bytes, que en bytes cabría
// a 260 000 por segundo, se queda en eso.
const bytesPorTramaMin = 512

// opsRedMin es el suelo del tope de tramas, para los caudales pequeños: el
// tráfico interactivo (ACK, DNS) no debería notarlo nunca.
const opsRedMin = 8192

// opsRed es el tope de tramas por segundo para un caudal de bps bytes/s.
func opsRed(bps int64) int64 {
	return max(bps/bytesPorTramaMin, opsRedMin)
}

// caudalRedDe es el caudal (bytes/s, en cada sentido) para la máquina id, y si
// es suyo: lo pidió, o lleva carpetas. Si no, es el del daemon, el mismo que
// se le dio a su snapshot.
func (m *Manager) caudalRedDe(id string) (bps int64, propio bool) {
	m.mu.RLock()
	var pedido int
	var carpetas bool
	if mc := m.byID[id]; mc != nil {
		pedido, carpetas = mc.NetMiBps, len(mc.Shares) > 0
	}
	m.mu.RUnlock()
	bps = caudalRed(pedido, m.netMiBps)
	// El suelo de las carpetas sustituye al caudal de siempre, no a uno que
	// alguien eligió: un daemon.net_mibps menor puesto a propósito se respeta.
	if pedido <= 0 && carpetas && !caudalDelDaemon(m.netMiBps) {
		bps = max(bps, netMiBpsCarpetas<<20)
	}
	return bps, pedido > 0 || carpetas
}

// caudalDelDaemon dice si el daemon tiene un caudal propio válido
// (daemon.net_mibps o KLING_NET_MIBPS), y no el de siempre.
func caudalDelDaemon(daemon func() int) bool {
	if daemon == nil {
		return false
	}
	n := daemon()
	return n > 0 && n <= config.MaxNetMiBps
}

// netMiBpsCarpetas es el caudal mínimo de una máquina con carpetas que no pide
// otro: viajan por su red, y con los 16 MiB/s de siempre un `cp` de la carpeta
// iba a 16 MiB/s. El CLI ya lo pedía (run -share, compose); decidirlo aquí lo
// da también a lo que arranca por el API (gateway, sandbox) y a las plantillas
// con carpetas, cuyas copias heredan el limitador de su snapshot.
const netMiBpsCarpetas = 256

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
