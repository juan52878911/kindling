package share

import (
	"container/list"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	proto "github.com/juan52878911/kindling/internal/shareproto"
)

// watcherDePrueba es un watcher sin backend real que apunta lo que manda.
func watcherDePrueba(t *testing.T, srv *Server) (*watcher, func() []string) {
	var mu sync.Mutex
	var got []string
	w := &watcher{srv: srv, be: mudo{}, entries: map[string]*watchEntry{}, lru: list.New(),
		echo: map[string]time.Time{}, pend: map[string]rawEvent{},
		push: func(b []byte) {
			d := proto.NewDec(b)
			_, _ = d.U64(), d.U32()
			ev, p, err := proto.ParsePush(d)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			got = append(got, string("?MCD"[ev])+" "+p)
			mu.Unlock()
		}}
	return w, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func heapVivo() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// S2: muchos nombres (enlaces) de UN directorio grande no guardan una foto
// cada uno: el heap del daemon no crece con los alias.
func TestWatchAliasNoMultiplicaFotos(t *testing.T) {
	dir := t.TempDir()
	grande := filepath.Join(dir, "grande")
	must(t, os.Mkdir(grande, 0o755))
	for i := range maxDirSnapshot {
		f, err := os.Create(filepath.Join(grande, fmt.Sprintf("fichero-con-nombre-largo-%06d", i)))
		must(t, err)
		f.Close()
	}
	const alias = 60
	for i := range alias {
		must(t, os.Symlink("grande", filepath.Join(dir, fmt.Sprintf("a%d", i))))
	}
	srv, err := Open(dir, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	w, _ := watcherDePrueba(t, srv)
	defer w.close()

	antes := heapVivo()
	w.note("grande", true)
	una := heapVivo() - antes
	for i := range alias {
		w.note(fmt.Sprintf("a%d", i), true)
	}
	crece := int64(heapVivo()) - int64(antes)
	t.Logf("heap: una foto %.2f MiB; con %d alias %.2f MiB", float64(una)/(1<<20), alias, float64(crece)/(1<<20))
	if crece > 3*int64(una)+(4<<20) {
		t.Fatalf("el heap creció %d MiB con %d alias de un directorio (una foto: %d MiB)", crece>>20, alias, una>>20)
	}
	runtime.KeepAlive(w)
}

// S2: la suma de las fotos de una sesión tiene tope. Pasado el tope un
// directorio no guarda foto, y un cambio en él se avisa del directorio entero.
func TestWatchFotosConTope(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"d1", "d2", "d3"} {
		must(t, os.Mkdir(filepath.Join(dir, d), 0o755))
		for i := range 100 {
			must(t, os.WriteFile(filepath.Join(dir, d, fmt.Sprintf("f%03d", i)), nil, 0o644))
		}
	}
	viejo := maxSnapBytes
	maxSnapBytes = 2 * 100 * (4 + snapEntryCost) // caben dos de los tres
	defer func() { maxSnapBytes = viejo }()

	srv, err := Open(dir, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	w, got := watcherDePrueba(t, srv)
	defer w.close()
	for _, d := range []string{"d1", "d2", "d3"} {
		w.note(d, true)
	}
	w.mu.Lock()
	usado, sinFoto := w.snapBytes, w.entries["d3"].snap == nil
	w.mu.Unlock()
	if usado > maxSnapBytes || !sinFoto {
		t.Fatalf("fotos: %d bytes (tope %d), d3 sin foto: %v", usado, maxSnapBytes, sinFoto)
	}

	must(t, os.WriteFile(filepath.Join(dir, "d3", "nuevo"), nil, 0o644))
	w.event(rawEvent{rel: "d3", dir: true})
	deadline := time.Now().Add(2 * time.Second)
	for len(got()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if g := got(); len(g) != 1 || g[0] != "M d3" {
		t.Fatalf("pushes %v, want [M d3]", g)
	}

	// Al soltar una foto, su sitio vuelve al presupuesto.
	w.mu.Lock()
	w.dropLocked(w.entries["d1"])
	usado = w.snapBytes
	w.mu.Unlock()
	if usado > maxSnapBytes/2 {
		t.Fatalf("tras soltar d1 siguen %d bytes", usado)
	}
}
