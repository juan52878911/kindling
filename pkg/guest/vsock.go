package guest

// El agente por virtio-vsock (docs/backend-vz.md).
//
// En macOS, kling-vz arranca las máquinas con un virtio-vsock y añade
// kling.vsock=1 a la línea de comandos: entonces el agente escucha también por
// AF_VSOCK en su puerto, y kling-vz le lleva por ahí el tráfico del núcleo
// (exec, ficheros, /resync, sondas) en vez de por la red de usuario. Sin el
// parámetro (Firecracker, o un kling-vz anterior) no cambia nada: solo TCP.
// Y un host nuevo con un agente anterior tampoco: el agente ignora el
// parámetro y kling-vz, al no poder conectar por vsock, sigue por TCP.

import (
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"time"
)

// vsockBootParam pide al agente que escuche también por vsock.
const vsockBootParam = "kling.vsock"

// VsockRequested dice si el host pidió el canal por vsock (kling.vsock=1).
func VsockRequested() bool {
	return vsockRequested(cmdlineParams())
}

func vsockRequested(p bootParams) bool {
	return p.values[vsockBootParam] == "1"
}

// ServeVsock sirve srv también por AF_VSOCK en port si el host lo pidió.
// Best-effort: si el kernel no deja (sin dispositivo, sin soporte), se avisa
// y el agente sigue solo por TCP. srv.Shutdown cierra también este listener.
func ServeVsock(srv *http.Server, port uint32) {
	if !VsockRequested() {
		return
	}
	l, err := ListenVsock(port)
	if err != nil {
		log.Printf("warning: the host asked for the vsock channel but it can't be opened (%v): serving over TCP only", err)
		return
	}
	log.Printf("also listening on vsock port %d", port)
	go func() {
		if err := srv.Serve(l); err != nil && err != http.ErrServerClosed {
			log.Printf("vsock listener: %v", err)
		}
	}()
}

// vsockAddr es la dirección de un extremo vsock (CID y puerto).
type vsockAddr struct{ cid, port uint32 }

func (a vsockAddr) Network() string { return "vsock" }
func (a vsockAddr) String() string {
	return strconv.FormatUint(uint64(a.cid), 10) + ":" + strconv.FormatUint(uint64(a.port), 10)
}

var _ net.Addr = vsockAddr{}

// vsockConn es una conexión vsock aceptada: un *os.File no bloqueante
// (lectura, escritura y plazos por el netpoller) con lo que le falta para ser
// un net.Conn.
//
// Los errores salen como *net.OpError, igual que los de un *net.TCPConn, y no
// como los *fs.PathError de *os.File. No es cosmético: net/http aborta la
// lectura de fondo de cada petición con un plazo vencido y solo la reconoce si
// el error es un net.Error con Timeout(); un *fs.PathError no lo es (le falta
// Temporary), así que la daba por un fallo de la conexión y cancelaba el
// contexto de TODAS las peticiones siguientes por la misma conexión. Con el
// keep-alive del daemon, cada exec tras el primero fallaba con "context
// canceled".
type vsockConn struct {
	f     *os.File
	local vsockAddr
	// remote es el otro extremo, de getpeername: el host es el CID 2
	// (VMADDR_CID_HOST); un proceso del propio invitado por el loopback de
	// vsock sería el 1.
	remote vsockAddr
}

// vmaddrCIDHost es el CID del host (VMADDR_CID_HOST).
const vmaddrCIDHost = 2

func newVsockConn(f *os.File, local vsockAddr) *vsockConn {
	return &vsockConn{f: f, local: local, remote: vsockAddr{cid: vmaddrCIDHost}}
}

// opErr traduce un error de *os.File al que daría el paquete net.
func (c *vsockConn) opErr(op string, err error) error {
	if err == nil || err == io.EOF {
		return err
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	if errors.Is(err, os.ErrClosed) {
		err = net.ErrClosed
	}
	return &net.OpError{Op: op, Net: "vsock", Source: c.local, Addr: c.RemoteAddr(), Err: err}
}

func (c *vsockConn) Read(b []byte) (int, error) {
	n, err := c.f.Read(b)
	return n, c.opErr("read", err)
}

func (c *vsockConn) Write(b []byte) (int, error) {
	n, err := c.f.Write(b)
	return n, c.opErr("write", err)
}

func (c *vsockConn) Close() error        { return c.opErr("close", c.f.Close()) }
func (c *vsockConn) LocalAddr() net.Addr { return c.local }

// RemoteAddr: el otro extremo (el host, CID 2, salvo que getpeername diga
// otra cosa).
func (c *vsockConn) RemoteAddr() net.Addr { return c.remote }

func (c *vsockConn) SetDeadline(t time.Time) error     { return c.opErr("set", c.f.SetDeadline(t)) }
func (c *vsockConn) SetReadDeadline(t time.Time) error { return c.opErr("set", c.f.SetReadDeadline(t)) }
func (c *vsockConn) SetWriteDeadline(t time.Time) error {
	return c.opErr("set", c.f.SetWriteDeadline(t))
}

// CloseWrite: el medio cierre, como el de un *net.TCPConn.
func (c *vsockConn) CloseWrite() error {
	rc, err := c.f.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = syscall.Shutdown(int(fd), syscall.SHUT_WR) }); err != nil {
		return err
	}
	return c.opErr("close", serr)
}

var _ net.Conn = (*vsockConn)(nil)
