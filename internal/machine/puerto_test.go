package machine

import (
	"net"
	"sync/atomic"
	"testing"
)

// dirSinNadie es una dirección en la que nadie escucha: el puerto 1 (tcpmux)
// no lo da nunca un Listen en :0 —los efímeros empiezan muy por encima— ni lo
// pide ningún test, así que quien marque recibe un RST.
// Abrir un listener en :0 y cerrarlo para «tener un puerto cerrado» devuelve
// el puerto al sistema, y con go test corriendo paquetes en paralelo otro
// proceso puede escuchar en él justo después y recibir las peticiones de este
// test (así le caían POST de resync a los tests del proxy MySQL de
// pkg/credproxy, que los registraban como bad_startup).
const dirSinNadie = "127.0.0.1:1"

// listenerTardio escucha desde ya en 127.0.0.1:0 —el puerto es nuestro todo
// el rato— pero corta con RST lo que llega hasta que se llama a abrir: para
// quien marca es como si nadie escuchase todavía.
type listenerTardio struct {
	net.Listener
	abierto atomic.Bool
}

func nuevoListenerTardio(t *testing.T) *listenerTardio {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return &listenerTardio{Listener: ln}
}

func (l *listenerTardio) abrir() { l.abierto.Store(true) }

func (l *listenerTardio) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil || l.abierto.Load() {
			return c, err
		}
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		c.Close()
	}
}
