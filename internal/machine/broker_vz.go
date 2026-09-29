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
// QUIÉN: el otro extremo tiene que ser de este mismo usuario (LOCAL_PEERCRED),
// su PID (LOCAL_PEERPID, los dos los pone el kernel al conectar) tiene que
// ser un proceso cuyo ejecutable es el kling-vz del daemon (proc_pidpath, por
// la llamada proc_info: sin cgo) y el VMM de exactamente una máquina.
// Mientras una máquina arranca o se descongela, su PID llega al manager un
// poco después de que el invitado corra: se espera hasta plazoIdentificar,
// pero como mucho maxEsperandoIdentidad conexiones a la vez; las demás se
// rechazan en el acto.

import (
	"bytes"
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
	"unsafe"

	"github.com/juan52878911/kindling/internal/fc"
	"github.com/juan52878911/kindling/pkg/api"
)

const (
	// solLocal, localPeerCred y localPeerPID son SOL_LOCAL, LOCAL_PEERCRED y
	// LOCAL_PEERPID de <sys/un.h>.
	solLocal      = 0
	localPeerCred = 0x001
	localPeerPID  = 0x002
	// xucredVersion es XUCRED_VERSION de <sys/ucred.h>.
	xucredVersion = 0
	// procInfoCallPIDInfo y procPIDPathInfo son PROC_INFO_CALL_PIDINFO y
	// PROC_PIDPATHINFO (lo que hace proc_pidpath de libproc por dentro);
	// procPIDPathMax es PROC_PIDPATHINFO_MAXSIZE (4*MAXPATHLEN).
	procInfoCallPIDInfo = 2
	procPIDPathInfo     = 11
	procPIDPathMax      = 4 * 1024
	// maxAtendiendo acota las conexiones al broker a la vez.
	maxAtendiendo = 4096
	// maxEsperandoIdentidad acota las que esperan a que su PID sea el de una
	// máquina (arranque o thaw en curso): cada una puede durar
	// plazoIdentificar, y 4096 esperando serían 4096 goroutines y descriptores
	// retenidos por nada.
	maxEsperandoIdentidad = 64
)

// esperandoIdentidad son los huecos de maxEsperandoIdentidad.
var esperandoIdentidad = make(chan struct{}, maxEsperandoIdentidad)

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
	uid, err := uidDelOtro(c)
	if err != nil {
		return "", err
	}
	if uid != os.Geteuid() {
		return "", fmt.Errorf("the peer runs as uid %d, not as the daemon's user", uid)
	}
	pid, err := pidDelOtro(c)
	if err != nil {
		return "", err
	}
	if err := m.esKlingVZ(pid); err != nil {
		return "", err
	}
	if id, n := m.maquinaDePID(pid); n == 1 {
		return id, nil
	}
	// Hay que esperar (o fallar): solo unas pocas a la vez.
	select {
	case esperandoIdentidad <- struct{}{}:
		defer func() { <-esperandoIdentidad }()
	default:
		return "", fmt.Errorf("pid %d is not the VMM of any machine yet and too many connections are waiting", pid)
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

// esKlingVZ comprueba que el ejecutable del proceso pid es el kling-vz con el
// que el daemon arranca las máquinas (m.fcBin): la misma ruta si es absoluta
// (tras resolver enlaces), o el mismo nombre si se buscó en el PATH. Cierra
// el caso de un PID de una máquina muerta que el sistema recicla para otro
// programa del mismo usuario antes de que el vigilante lo note.
func (m *Manager) esKlingVZ(pid int) error {
	ruta, err := rutaEjecutable(pid)
	if err != nil {
		return fmt.Errorf("pid %d: %w", pid, err)
	}
	m.mu.RLock()
	bin := m.fcBin // fijo en producción; los tests lo cambian bajo m.mu
	m.mu.RUnlock()
	if !mismoEjecutable(ruta, bin) {
		return fmt.Errorf("pid %d runs %s, not the VMM", pid, ruta)
	}
	return nil
}

// mismoEjecutable dice si ruta (la de proc_pidpath, ya resuelta) es bin.
func mismoEjecutable(ruta, bin string) bool {
	if bin == "" || ruta == "" {
		return false
	}
	if !filepath.IsAbs(bin) {
		return filepath.Base(ruta) == filepath.Base(bin)
	}
	if r, err := filepath.EvalSymlinks(bin); err == nil {
		bin = r
	}
	return filepath.Clean(ruta) == filepath.Clean(bin)
}

// rutaEjecutable es la ruta del ejecutable del proceso pid (proc_pidpath de
// libproc, por la llamada proc_info: el núcleo no usa cgo).
func rutaEjecutable(pid int) (string, error) {
	buf := make([]byte, procPIDPathMax)
	// Como libproc: el retorno no es la longitud; la ruta acaba en NUL.
	_, _, e := syscall.Syscall6(syscall.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(pid), procPIDPathInfo, 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if e != 0 {
		return "", fmt.Errorf("proc_pidpath: %w", e)
	}
	n := bytes.IndexByte(buf, 0)
	if n <= 0 {
		return "", errors.New("proc_pidpath returned nothing")
	}
	return string(buf[:n]), nil
}

// xucred es struct xucred de <sys/ucred.h>.
type xucred struct {
	Version uint32
	UID     uint32
	NGroups int16
	_       int16
	Groups  [16]uint32
}

// uidDelOtro es el UID efectivo del proceso que conectó c (LOCAL_PEERCRED).
func uidDelOtro(c *net.UnixConn) (int, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var x xucred
	var serr error
	if err := rc.Control(func(fd uintptr) {
		l := uint32(unsafe.Sizeof(x))
		_, _, e := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, solLocal, localPeerCred,
			uintptr(unsafe.Pointer(&x)), uintptr(unsafe.Pointer(&l)), 0)
		if e != 0 {
			serr = e
		} else if l < 8 {
			serr = fmt.Errorf("short xucred (%d bytes)", l)
		}
	}); err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, fmt.Errorf("LOCAL_PEERCRED: %w", serr)
	}
	if x.Version != xucredVersion {
		return 0, fmt.Errorf("LOCAL_PEERCRED: xucred version %d", x.Version)
	}
	return int(x.UID), nil
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
