//go:build !darwin

package daemon

import (
	"log"
	"path/filepath"
	"strings"

	knet "github.com/juan52878911/kindling/internal/net"
)

// cederSocket: en Linux el daemon es root y el CLI entra como usuario, así
// que el socket se cede (ver socketOwner).
const cederSocket = true

// construirImagenes: los constructores montan un loopback y hacen chroot, que
// solo existe aquí.
const construirImagenes = true

// prepararRaizPlataforma fija <raíz>/credtmp como directorio de derrame del
// proxy de credenciales (un cuerpo grande con la clave dentro, ver
// pkg/credproxy/cuerpo.go): 0700 del daemon y vaciado de lo que dejó uno
// anterior muerto a mitad. Si no se puede, el proxy no escribe a disco.
func prepararRaizPlataforma(root string) {
	if err := knet.SetCredTempDir(filepath.Join(root, "credtmp")); err != nil {
		log.Printf("WARNING: credential proxy spill directory: %v (large request bodies will be sent chunked)", err)
	}
}

// comprobarHost avisa de lo que falta para arrancar microVMs y monta las
// reglas de red del host.
//
// Los externos que hacen falta para arrancar una microVM. Solo se comprobaba la
// red (`ip`, `iptables`), asi que el daemon se quedaba escuchando y aceptando
// peticiones sin `firecracker`, sin `setpriv` y sin `mkfs.ext4`. `setpriv` era el
// peor: no lo cubria ninguna comprobacion y fallaba por microVM en pleno arranque.
//
// Sigue siendo AVISO y no error: el daemon vale para inspeccionar estado aunque no
// pueda arrancar nada. Pero ahora los nombra TODOS de golpe.
func (s *Server) comprobarHost() {
	if missing := missingBinaries(); len(missing) > 0 {
		log.Printf("WARNING: missing binaries (%s): microVMs cannot be started on this host",
			strings.Join(missing, ", "))
	}

	if err := knet.Available(); err != nil {
		log.Printf("WARNING: network unavailable (%v): microVMs will boot without connectivity", err)
	} else if err := knet.SetupHost(); err != nil {
		log.Printf("WARNING: couldn't install the host barrier rules: %v", err)
	}
}
