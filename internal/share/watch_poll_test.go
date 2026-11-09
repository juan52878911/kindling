package share

import (
	"container/list"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	proto "github.com/juan52878911/kindling/internal/shareproto"
)

// recogedor junta los avisos que el watcher manda al agente.
type recogedor struct {
	t   *testing.T
	mu  sync.Mutex
	got []string
}

func (r *recogedor) push(b []byte) {
	d := proto.NewDec(b)
	_, _ = d.U64(), d.U32()
	ev, p, err := proto.ParsePush(d)
	if err != nil {
		r.t.Error(err)
	}
	r.mu.Lock()
	r.got = append(r.got, string("?MCD"[ev])+" "+p)
	r.mu.Unlock()
}

// espera hasta que lleguen n avisos (o pasa el plazo) y los devuelve en orden.
func (r *recogedor) espera(n int, plazo time.Duration) []string {
	deadline := time.Now().Add(plazo)
	for {
		r.mu.Lock()
		k := len(r.got)
		r.mu.Unlock()
		if k >= n || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.got...)
	sort.Strings(out)
	return out
}

// Por sondeo, sin ningún evento del sistema, los cambios del host llegan igual.
func TestVigilanciaPorSondeo(t *testing.T) {
	old := pollInterval
	pollInterval = 20 * time.Millisecond
	defer func() { pollInterval = old }()

	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("v1"), 0o644))
	srv, err := Open(dir, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	r := &recogedor{t: t}
	w := &watcher{srv: srv, push: r.push, entries: map[string]*watchEntry{}, lru: list.New(),
		echo: map[string]time.Time{}, pend: map[string]rawEvent{}, polling: true}
	w.be = newPollBackend(w.event)
	defer w.close()
	w.note("", true)
	w.note("f.txt", false)

	must(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("version 2"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "nuevo"), nil, 0o644))
	want := []string{"C nuevo", "M f.txt"}
	got := r.espera(len(want), 2*time.Second)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("pushes %v, want %v", got, want)
	}
}
