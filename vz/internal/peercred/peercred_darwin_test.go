//go:build darwin

package peercred

import (
	"net"
	"testing"
	"time"
)

// Una conexión que abre este mismo proceso es de nuestro usuario: pasa, y la
// segunda vez sale de la caché de procesos recientes.
func TestConexionPropiaPasa(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	k := New()
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp4", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		s, err := l.Accept()
		if err != nil {
			t.Fatal(err)
		}
		inicio := time.Now()
		ok := k.Allowed(s)
		t.Logf("vuelta %d: %v en %s", i, ok, time.Since(inicio))
		if !ok {
			t.Fatalf("vuelta %d: una conexión de este mismo proceso no pasó", i)
		}
		c.Close()
		s.Close()
	}
}

// Un socket cuyo otro extremo no tiene ningún proceso nuestro (aquí: un
// puerto inventado) no pasa.
func TestSinDuenoNoPasa(t *testing.T) {
	k := New()
	falsa := conexionFalsa{local: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}, remoto: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2}}
	inicio := time.Now()
	if k.Allowed(falsa) {
		t.Fatal("una conexión sin dueño entre nuestros procesos pasó")
	}
	t.Logf("recorrido completo sin dueño: %s", time.Since(inicio))
}

type conexionFalsa struct {
	net.Conn
	local, remoto net.Addr
}

func (c conexionFalsa) LocalAddr() net.Addr  { return c.local }
func (c conexionFalsa) RemoteAddr() net.Addr { return c.remoto }
