package share

import (
	"container/list"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	proto "github.com/juan52878911/kindling/pkg/share"
)

// mudo es un backend que no cuenta nada: lo que pase en el host se pierde,
// como con la cola de inotify desbordada.
type mudo struct{}

func (mudo) add(string, bool) error { return nil }
func (mudo) remove(string)          {}
func (mudo) close()                 {}

// B9: tras IN_Q_OVERFLOW el watcher repasa lo vigilado y avisa exactamente de
// lo que cambió mientras se perdían eventos, y de nada más.
func TestOverflowLoRepasaTodo(t *testing.T) {
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	for _, n := range []string{"f.txt", "g.txt", "quieto.txt"} {
		must(t, os.WriteFile(filepath.Join(dir, n), []byte("v1"), 0o644))
	}
	srv, err := Open(dir, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

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
	defer w.close()
	for _, n := range []string{"", "sub"} {
		w.note(n, true)
	}
	for _, n := range []string{"f.txt", "g.txt", "quieto.txt"} {
		w.note(n, false)
	}

	must(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("version 2"), 0o644))
	must(t, os.Remove(filepath.Join(dir, "g.txt")))
	must(t, os.WriteFile(filepath.Join(dir, "sub", "nuevo"), nil, 0o644))
	w.event(rawEvent{overflow: true})

	want := []string{"C sub/nuevo", "D g.txt", "M f.txt"}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= len(want) || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(3 * watchDebounce) // por si llegara algo de más
	mu.Lock()
	defer mu.Unlock()
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("pushes %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pushes %v, want %v", got, want)
		}
	}
}
