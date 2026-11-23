package share

import (
	"container/list"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	proto "github.com/juan52878911/kindling/internal/shareproto"
)

// avisoOlvido es el código del aviso «deja de fiarte de esta ruta» (ver
// EventForget en watch.go).
const avisoOlvido = 4

type aviso struct {
	ev   byte
	path string
}

// watcherCrudo es un watcher con backend mudo que apunta cada aviso tal cual,
// también los que ParsePush no conoce.
func watcherCrudo(t *testing.T, srv *Server) (*watcher, func() []aviso) {
	var mu sync.Mutex
	var got []aviso
	w := &watcher{srv: srv, be: mudo{}, entries: map[string]*watchEntry{}, lru: list.New(),
		echo: map[string]time.Time{}, pend: map[string]rawEvent{},
		push: func(b []byte) {
			d := proto.NewDec(b)
			_, _ = d.U64(), d.U32()
			ev, p := d.U8(), d.Str()
			if d.Done() != nil {
				t.Errorf("malformed push %x", b)
			}
			mu.Lock()
			got = append(got, aviso{ev, p})
			mu.Unlock()
		}}
	t.Cleanup(w.close)
	return w, func() []aviso {
		mu.Lock()
		defer mu.Unlock()
		return append([]aviso(nil), got...)
	}
}

func esperarAviso(t *testing.T, got func() []aviso, want aviso) {
	t.Helper()
	tope := time.Now().Add(3 * time.Second)
	for time.Now().Before(tope) {
		for _, a := range got() {
			if a == want {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no push %+v; got %+v", want, got())
}

func conTope(t *testing.T, n int) {
	viejo := maxWatchesPerSession
	maxWatchesPerSession = n
	t.Cleanup(func() { maxWatchesPerSession = viejo })
}

// Camino 1: una ruta expulsada por el tope deja de vigilarse, pero el kernel
// del invitado la tenía con la caché larga. El daemon tiene que decírselo, y
// la raíz no se expulsa (olvidarla es invalidarlo todo).
func TestExpulsadaSeOlvidaEnElInvitado(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		must(t, os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644))
	}
	srv, err := Open(dir, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	conTope(t, 3)
	w, got := watcherCrudo(t, srv)
	w.note("", true)
	w.note("a", false)
	w.note("b", false)
	w.note("c", false) // expulsa a la menos reciente que no es la raíz: a

	esperarAviso(t, got, aviso{avisoOlvido, "a"})
	w.mu.Lock()
	raiz := w.entries[""] != nil
	w.mu.Unlock()
	if !raiz {
		t.Error("the root was evicted")
	}
	for _, a := range got() {
		if a.ev == avisoOlvido && a.path != "a" {
			t.Errorf("unexpected forget %+v", a)
		}
	}
}

// Camino 3: un stat que da ENOENT deja en el invitado una entrada negativa
// larga bajo el padre. El padre tiene que seguir vigilado: el ENOENT lo
// refresca en la LRU, como cualquier otra cosa que el invitado mira.
func TestENOENTRefrescaAlPadre(t *testing.T) {
	dir := t.TempDir()
	must(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
	for _, n := range []string{"f1", "f2", "f3"} {
		must(t, os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644))
	}
	srv, err := Open(dir, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	conTope(t, 3)
	w, _ := watcherCrudo(t, srv)
	ss := &session{srv: srv, watch: w}
	stat := func(p string) uint32 {
		e := &proto.Enc{}
		e.Str(p)
		d := proto.NewDec(ss.dispatch(1, proto.OpStat, proto.NewDec(e.B)))
		_ = d.U64()
		return d.U32()
	}
	for _, p := range []string{"sub", "f1", "f2"} {
		if e := stat(p); e != 0 {
			t.Fatalf("stat %s: %d", p, e)
		}
	}
	if e := stat("sub/no-existe"); e != proto.ENOENT {
		t.Fatalf("stat sub/no-existe: %d", e)
	}
	stat("f3") // expulsa a la menos reciente: ya no debe ser sub
	w.mu.Lock()
	sigue := w.entries["sub"] != nil
	w.mu.Unlock()
	if !sigue {
		t.Fatal("sub is no longer watched after a negative lookup under it")
	}
}
