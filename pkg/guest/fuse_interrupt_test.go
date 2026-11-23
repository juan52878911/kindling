package guest

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

// conectarMudo publica una sesión cuyo daemon lee las peticiones y no contesta
// ninguna, como uno colgado en una E/S del host. corta cierra la conexión.
func (m *montaje) conectarMudo(t *testing.T) (corta func()) {
	t.Helper()
	a, b := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, a) }()
	m.fs.attach(newShareSession(b))
	return func() { a.Close(); b.Close() }
}

// enviar manda una petición sin esperar la respuesta; esta llega por el canal.
func (d *devFalso) enviar(op uint32, nodeid uint64, arg ...[]byte) (uint64, chan []byte) {
	d.mu.Lock()
	d.unique++
	u := d.unique
	ch := make(chan []byte, 1)
	d.outs[u] = ch
	d.mu.Unlock()
	d.in <- mkReq(op, u, nodeid, arg...)
	return u, ch
}

func errnoDe(r []byte) uint32 {
	return uint32(-int32(binary.LittleEndian.Uint32(r[4:])))
}

// Un proceso bloqueado en una lectura de la carpeta tiene que poder morir: el
// kernel manda FUSE_INTERRUPT y espera la respuesta de la petición original.
// Si el agente se la calla, el proceso queda en D hasta que vuelva el daemon.
func TestInterruptContestaUnaLecturaColgada(t *testing.T) {
	m := montar(t, carpeta(t), false)
	e, id, _ := m.lookup(t, rootID, "hello.txt")
	if e != 0 {
		t.Fatalf("lookup: %d", e)
	}
	m.stop()
	corta := m.conectarMudo(t)
	m.stop = corta

	u, ch := m.dev.enviar(opGetattr, id, u32s(0, 0), u64s(0))
	// Que la petición llegue a la sesión antes de interrumpirla.
	time.Sleep(100 * time.Millisecond)
	m.dev.soltar(opInterrupt, 0, u64s(u))
	select {
	case r := <-ch:
		if e := errnoDe(r); e != 4 { // EINTR de Linux
			t.Fatalf("interrupted getattr answered errno %d, want EINTR", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("an interrupted request on a hung session was never answered")
	}
}

// Una operación de solo lectura que ya salió cuando la sesión se corta no se
// hizo a medias: se puede repetir con la sesión siguiente en vez de dar EIO.
func TestCorteRepiteLasIdempotentes(t *testing.T) {
	m := montar(t, carpeta(t), false)
	e, id, _ := m.lookup(t, rootID, "hello.txt")
	if e != 0 {
		t.Fatalf("lookup: %d", e)
	}
	m.stop()
	corta := m.conectarMudo(t)

	_, ch := m.dev.enviar(opGetattr, id, u32s(0, 0), u64s(0))
	time.Sleep(100 * time.Millisecond)
	corta()
	m.stop = m.conectar(t)
	select {
	case r := <-ch:
		if e := errnoDe(r); e != 0 {
			t.Fatalf("a getattr in flight during a cut answered errno %d, want a retry on the next session", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
	}
}
