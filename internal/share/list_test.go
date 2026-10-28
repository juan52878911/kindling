package share

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	proto "github.com/juan52878911/kindling/pkg/share"
)

// listar pide las páginas de p desde cookie con páginas de max bytes, como el
// opendir del agente, y devuelve los nombres.
func (c *cliente) listar(t *testing.T, p string, cookie uint64, max uint32, paginas int) ([]string, uint64, bool) {
	t.Helper()
	var nombres []string
	for i := 0; paginas < 0 || i < paginas; i++ {
		e, d := c.call(proto.OpList, func(e *proto.Enc) { e.Str(p); e.U64(cookie); e.U32(max) })
		if e != 0 {
			t.Fatalf("list %q@%d: %d", p, cookie, e)
		}
		eof, n := d.U8(), d.U32()
		for j := uint32(0); j < n; j++ {
			name, _ := d.Str(), d.U32()
			nombres = append(nombres, name)
		}
		next := d.U64()
		if d.Done() != nil {
			t.Fatal("malformed list reply")
		}
		if eof != 0 {
			return nombres, next, true
		}
		if next <= cookie {
			t.Fatalf("list does not advance: %d -> %d", cookie, next)
		}
		cookie = next
	}
	return nombres, cookie, false
}

// Un directorio grande listado en páginas pequeñas sale entero y sin
// repetidos, siga el cursor guardado o no; y al acabar no queda ningún
// descriptor del presupuesto ocupado.
func TestListPorPaginas(t *testing.T) {
	dir := t.TempDir()
	const total = 3000
	for i := 0; i < total; i++ {
		f, err := os.Create(filepath.Join(dir, fmt.Sprintf("fichero-%05d", i)))
		must(t, err)
		f.Close()
	}
	// Uno que no se enseña: cuenta para la cookie pero no sale.
	must(t, os.Symlink("x", filepath.Join(dir, "enlace")))

	srv, err := Open(dir, true, t.Name())
	must(t, err)
	defer srv.Close()
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, a); close(done) }()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); b.Close(); <-done }) }
	defer stop()
	c := &cliente{t: t, conn: b}

	comprobar := func(nombres []string) {
		t.Helper()
		vistos := map[string]bool{}
		for _, n := range nombres {
			if vistos[n] {
				t.Fatalf("%q listed twice", n)
			}
			vistos[n] = true
		}
		// total ficheros y el enlace (los enlaces sí se enseñan).
		if len(vistos) != total+1 {
			t.Fatalf("listed %d entries, want %d", len(vistos), total+1)
		}
	}

	// Seguido, con el cursor.
	nombres, _, eof := c.listar(t, "", 0, 2048, -1)
	if !eof {
		t.Fatal("no eof")
	}
	comprobar(nombres)

	// A medias, y luego dos listados a la vez que se cruzan: cada uno sigue el
	// suyo por su cookie.
	p1, k1, _ := c.listar(t, "", 0, 2048, 3)
	p2, k2, _ := c.listar(t, "", 0, 2048, 1)
	r1, _, _ := c.listar(t, "", k1, 2048, -1)
	r2, _, _ := c.listar(t, "", k2, 2048, -1)
	comprobar(append(p1, r1...))
	comprobar(append(p2, r2...))

	// Una cookie sin cursor (de otra sesión, o tras caducar) reabre y salta.
	p3, k3, _ := c.listar(t, "", 0, 2048, 2)
	c2 := servir(t, dir, true)
	r3, _, _ := c2.listar(t, "", k3, 2048, -1)
	comprobar(append(p3, r3...))

	// El cursor de p3 se quedó en la primera sesión a medias: ocupa un
	// descriptor hasta que la sesión acaba. Los demás ya se soltaron.
	if n := len(srv.budget.slots); n != 1 {
		t.Errorf("%d descriptors held by cursors, want 1", n)
	}
	stop()
	if n := len(srv.budget.slots); n != 0 {
		t.Errorf("%d descriptors still held after the session ended", n)
	}
}
