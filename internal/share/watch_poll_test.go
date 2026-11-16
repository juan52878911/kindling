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

// Por sondeo, un directorio sin foto (presupuesto agotado) solo avisa cuando
// cambia: antes avisaba en cada repaso, cada pollInterval y para siempre.
func TestSondeoSinFotoSoloAvisaSiCambia(t *testing.T) {
	oldPoll, oldSnap := pollInterval, maxSnapBytes
	pollInterval, maxSnapBytes = 20*time.Millisecond, 0
	defer func() { pollInterval, maxSnapBytes = oldPoll, oldSnap }()

	dir := t.TempDir()
	must(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "sub", "a"), nil, 0o644))
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
	w.note("sub", true)
	w.mu.Lock()
	sinFoto := w.entries["sub"] != nil && w.entries["sub"].snap == nil
	w.mu.Unlock()
	if !sinFoto {
		t.Fatal("sub should be watched without a snapshot")
	}

	// Diez repasos sin tocar nada: ningún aviso.
	if got := r.espera(1, 10*pollInterval); len(got) != 0 {
		t.Fatalf("unchanged directory pushes %v, want none", got)
	}
	// Un nombre nuevo sí se avisa (del directorio, que no tiene foto).
	must(t, os.WriteFile(filepath.Join(dir, "sub", "b"), nil, 0o644))
	if got := r.espera(1, 2*time.Second); len(got) == 0 || got[0] != "M sub" {
		t.Fatalf("after a new name pushes %v, want [M sub]", got)
	}
}

// Con un invitado que no lee, los avisos no acumulan goroutines: hay como
// mucho un flush esperando en la escritura, y lo que llegó mientras se manda
// cuando esa escritura acaba.
func TestFlushUnoALaVez(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "f"), nil, 0o644))
	srv, err := Open(dir, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	var mu sync.Mutex
	dentro, maxDentro, total := 0, 0, 0
	soltar := make(chan struct{})
	push := func([]byte) {
		mu.Lock()
		dentro++
		total++
		maxDentro = max(maxDentro, dentro)
		mu.Unlock()
		<-soltar // el invitado no lee
		mu.Lock()
		dentro--
		mu.Unlock()
	}
	w := &watcher{srv: srv, push: push, entries: map[string]*watchEntry{}, lru: list.New(),
		echo: map[string]time.Time{}, pend: map[string]rawEvent{}, be: newPollBackend(func(rawEvent) {})}
	defer w.close()
	w.note("f", false)
	for range 20 {
		w.event(rawEvent{rel: "f"})
		time.Sleep(2 * watchDebounce)
	}
	mu.Lock()
	m := maxDentro
	mu.Unlock()
	if m != 1 {
		t.Fatalf("%d pushes waiting at once on a guest that does not read, want 1", m)
	}
	// Al soltar la escritura, lo pendiente sale en otro flush.
	close(soltar)
	plazo := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := total
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(plazo) {
			t.Fatalf("%d pushes after the write returned, want a second one for what was pending", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
