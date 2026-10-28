//go:build darwin

package vzvm

import (
	"context"
	"errors"
	"net"
	"unsafe"

	"github.com/Code-Hex/vz/v3"
)

// DialVsock abre una conexión virtio-vsock al puerto port del invitado. Es el
// canal con el agente: no pasa por la pila de usuario ni por virtio-net.
//
// Cada llamada es una conexión nueva, y no se guarda ninguna: tras un
// SaveState/RestoreState el dispositivo se reinicia
// (VIRTIO_VSOCK_EVENT_TRANSPORT_RESET) y las conexiones vivas mueren, pero
// el agente sigue escuchando y la siguiente llamada vuelve a conectar.
//
// Connect del framework es asíncrono y Code-Hex/vz espera su respuesta sin
// plazo: se espera aquí con ctx y, si vence, la conexión que llegue tarde se
// cierra al llegar.
func (m *machine) DialVsock(ctx context.Context, port uint32) (net.Conn, error) {
	devs := m.vm.SocketDevices()
	if len(devs) == 0 {
		return nil, errors.New("the VM has no vsock device")
	}
	dev := devs[0]
	type res struct {
		c   *vz.VirtioSocketConnection
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := dev.Connect(port)
		ch <- res{c, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		return wrapVsock(r.c), nil
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.c != nil {
				r.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// vsockConn añade CloseWrite a la conexión de Code-Hex/vz, que no la expone
// aunque su descriptor (un socket de dominio Unix) la tiene: el reenvío
// (vnet.splice) respeta el medio cierre, y sin ella cerraría la conexión
// entera en cuanto el cliente terminara de escribir.
type vsockConn struct {
	*vz.VirtioSocketConnection
	raw net.Conn
}

type closeWriter interface{ CloseWrite() error }

func (c *vsockConn) CloseWrite() error {
	if cw, ok := c.raw.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return errors.New("vsock connection has no half-close")
}

// wrapVsock saca la conexión cruda de c (su primer campo, rawConn, en
// Code-Hex/vz v3.7.1). Si la disposición cambia y no es un net.Conn, se
// devuelve c tal cual: sin medio cierre, pero funcionando.
func wrapVsock(c *vz.VirtioSocketConnection) net.Conn {
	raw := (*struct{ rawConn net.Conn })(unsafe.Pointer(c)).rawConn
	if raw == nil {
		return c
	}
	if _, ok := raw.(closeWriter); !ok {
		return c
	}
	return &vsockConn{VirtioSocketConnection: c, raw: raw}
}
