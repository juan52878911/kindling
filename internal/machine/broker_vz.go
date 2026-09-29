//go:build darwin

package machine

// El broker de enlaces en macOS: dónde escucha y cómo sabe quién pregunta
// (broker.go explica el porqué).
//
// DÓNDE: en /tmp/kling-<uid>/b-<hash de la raíz>.sock, el directorio privado
// del usuario que ya usa internal/fc para las rutas cortas (0700 y comprobado:
// nadie más crea nada ahí). No junto a la raíz: sun_path admite 104 bytes y
// ~/Library/Application Support/kindling se come la mitad; y el daemon no
// puede hacer chdir para atarse con un nombre corto, como kling-vz. La ruta
// es fija para una raíz: un kling-vz que sobrevive a un reinicio del daemon
// la sigue encontrando. El perfil de kling-vz (kling-vz.sb) solo le deja
// conectar a ese socket.
//
// QUIÉN: el PID del otro extremo (LOCAL_PEERPID, lo pone el kernel al
// conectar) tiene que ser el kling-vz de exactamente una máquina. Mientras
// una máquina arranca o se descongela, su PID llega al manager un poco
// después de que el invitado corra: se espera hasta plazoIdentificar.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	"github.com/juan52878911/kindling/pkg/api"
)

const (
	// solLocal y localPeerPID son SOL_LOCAL y LOCAL_PEERPID de <sys/un.h>.
	solLocal     = 0
	localPeerPID = 0x002
	// maxAtendiendo acota las conexiones al broker a la vez.
	maxAtendiendo = 4096
)

// plazoIdentificar: cuánto se espera a que el PID de quien pregunta sea el
// de una máquina (la que arranca o se descongela). Variable para las pruebas.
var plazoIdentificar = 15 * time.Second

// rutaBroker es el socket del broker para la raíz root.
func rutaBroker(root string) (string, error) {
	dir, err := fc.DirCorto()
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(filepath.Clean(root)))
	return filepath.Join(dir, "b-"+hex.EncodeToString(h[:8])+".sock"), nil
}

// iniciarBroker escucha en el socket del broker. Sin él no hay aristas ni
// attach en macOS (fallan cerradas), pero el daemon sigue: se avisa.
func (m *Manager) iniciarBroker() {
	ruta, err := rutaBroker(m.root)
	if err != nil {
		log.Printf("warning: link broker unavailable (graph edges and kling db attach won't work): %v", err)
		return
	}
	// Uno que sobra de un daemon anterior. Solo si es un socket.
	if fi, err := os.Lstat(ruta); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(ruta)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: ruta, Net: "unix"})
	if err != nil {
		log.Printf("warning: link broker unavailable (graph edges and kling db attach won't work): %v", err)
		return
	}
	_ = os.Chmod(ruta, 0o600)
	m.brokerLn, m.brokerRuta = ln, ruta
	go m.servirBroker(ln)
}

// cerrarBroker deja de escuchar (y el listener borra su socket).
func (m *Manager) cerrarBroker() {
	if m.brokerLn != nil {
		_ = m.brokerLn.Close()
	}
}

func (m *Manager) servirBroker(ln *net.UnixListener) {
	sem := make(chan struct{}, maxAtendiendo)
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		select {
		case sem <- struct{}{}:
		default:
			c.Close()
			continue
		}
		go func() {
			defer func() { <-sem }()
			origen, err := m.identificarBroker(c)
			if err != nil {
				log.Printf("link broker: refused a connection: %v", err)
				c.Close()
				return
			}
			m.atenderBroker(c, origen)
		}()
	}
}

// identificarBroker dice de qué máquina es el kling-vz al otro lado de c.
func (m *Manager) identificarBroker(c *net.UnixConn) (string, error) {
	pid, err := pidDelOtro(c)
	if err != nil {
		return "", err
	}
	limite := time.Now().Add(plazoIdentificar)
	for {
		id, n := m.maquinaDePID(pid)
		switch {
		case n == 1:
			return id, nil
		case n > 1:
			return "", fmt.Errorf("pid %d is recorded for %d machines", pid, n)
		case time.Now().After(limite):
			return "", fmt.Errorf("pid %d is not the VMM of any machine", pid)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// maquinaDePID busca la máquina cuyo VMM es pid; n dice cuántas lo tienen.
func (m *Manager) maquinaDePID(pid int) (id string, n int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, mc := range m.byID {
		if mc.PID == pid && (mc.State == api.StateRunning || mc.State == api.StatePaused) {
			id = mc.ID
			n++
		}
	}
	return id, n
}

// pidDelOtro es el PID del proceso que conectó c (LOCAL_PEERPID).
func pidDelOtro(c *net.UnixConn) (int, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var serr error
	if err := rc.Control(func(fd uintptr) {
		pid, serr = syscall.GetsockoptInt(int(fd), solLocal, localPeerPID)
	}); err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, fmt.Errorf("LOCAL_PEERPID: %w", serr)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("LOCAL_PEERPID gave %d", pid)
	}
	return pid, nil
}
