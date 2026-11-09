package guest

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	share "github.com/juan52878911/kindling/internal/shareproto"
)

// compuerta es el lado del host de la tubería, que se puede parar: con el
// host sin leer, lo escrito se queda pendiente en el agente.
type compuerta struct {
	net.Conn
	mu     sync.Mutex
	cond   sync.Cond
	parada bool
}

func (c *compuerta) Read(b []byte) (int, error) {
	c.mu.Lock()
	for c.parada {
		c.cond.Wait()
	}
	c.mu.Unlock()
	return c.Conn.Read(b)
}

func (c *compuerta) parar(p bool) {
	c.mu.Lock()
	c.parada = p
	c.mu.Unlock()
	c.cond.Broadcast()
}

// Lo pendiente de TODOS los ficheros no pasa de wbGlobalMax. El tope por nodo
// (wbMaxBytes) no basta: un `tar x` en una carpeta compartida eran 4 MiB por
// fichero abierto, sin suma acotada, en un invitado de 256 MiB.
func TestWriteBehindTopeGlobal(t *testing.T) {
	dir := carpeta(t)
	m := montar(t, dir, false)
	m.stop()
	a, b := net.Pipe()
	host := &compuerta{Conn: a}
	host.cond.L = &host.mu
	ctx, cancel := context.WithCancel(context.Background())
	hecho := make(chan struct{})
	go func() { _ = m.srv.Serve(ctx, host); close(hecho) }()
	s := newShareSession(b)
	s.hello()
	m.fs.attach(s)
	m.stop = func() { host.parar(false); cancel(); b.Close(); <-hecho }
	if s.caps&share.CapWriteBehind == 0 {
		t.Fatal("the daemon did not offer write-behind")
	}

	const ficheros, porFichero = 8, 4 // 8 × 4 MiB: el doble del tope global
	type abierto struct{ node, fh uint64 }
	var abiertos []abierto
	for i := 0; i < ficheros; i++ {
		e, out := m.dev.pedir(t, opCreate, rootID, u32s(linuxORdwr|linuxOCreat, 0o644, 0, 0), cstr(fmt.Sprintf("f%d", i)))
		if e != 0 {
			t.Fatalf("create: %d", e)
		}
		abiertos = append(abiertos, abierto{le.Uint64(out[0:]), le.Uint64(out[entryOutSize:])})
	}
	pendiente := func() int {
		total := 0
		for _, f := range abiertos {
			if st := m.fs.nodes.get(f.node).wb.Load(); st != nil {
				st.mu.Lock()
				total += st.bytes
				st.mu.Unlock()
			}
		}
		return total
	}

	base := wbGlobal.enUso()
	host.parar(true)
	var wg sync.WaitGroup
	for i, f := range abiertos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < porFichero; k++ {
				d := bytes.Repeat([]byte{byte('a' + i)}, share.MaxIO)
				m.dev.pedir(t, opWrite, f.node, writeIn(f.fh, uint64(k*share.MaxIO), len(d)), d)
			}
		}()
	}
	peor := 0
	for fin := time.Now().Add(time.Second); time.Now().Before(fin); time.Sleep(5 * time.Millisecond) {
		peor = max(peor, pendiente())
	}
	host.parar(false)
	wg.Wait()
	if peor > wbGlobalMax {
		t.Fatalf("%d MiB pending across files, global limit %d MiB", peor>>20, wbGlobalMax>>20)
	}
	for i, f := range abiertos {
		if e, _ := m.dev.pedir(t, opFlush, f.node, u64s(f.fh, 0, 0)); e != 0 {
			t.Fatalf("flush: %d", e)
		}
		got, _ := os.ReadFile(filepath.Join(dir, fmt.Sprintf("f%d", i)))
		if !bytes.Equal(got, bytes.Repeat([]byte{byte('a' + i)}, porFichero*share.MaxIO)) {
			t.Fatalf("f%d: the host has %d bytes", i, len(got))
		}
		m.dev.pedir(t, opRelease, f.node, u64s(f.fh, 0, 0))
	}
	// Lo reservado vuelve todo (otros tests pueden dejar algo suyo en vuelo:
	// se compara con lo que había al empezar).
	waitUntil(t, "the global budget to come back", func() bool { return wbGlobal.enUso() <= base })
}
