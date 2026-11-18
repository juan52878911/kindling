package share

import (
	"container/list"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// watcherSondeo es un watcher como el de una sesión que vigila por sondeo, con
// los directorios dirs ya vigilados (y su foto tomada).
func watcherSondeo(tb testing.TB, root string, dirs []string, push func([]byte)) *watcher {
	tb.Helper()
	srv, err := Open(root, false, "")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { srv.Close() })
	w := &watcher{srv: srv, be: mudo{}, entries: map[string]*watchEntry{}, lru: list.New(),
		echo: map[string]time.Time{}, pend: map[string]rawEvent{}, polling: true, push: push}
	tb.Cleanup(w.close)
	for _, d := range dirs {
		w.note(d, true)
	}
	return w
}

// repasar hace lo que cada pollInterval una sesión que sondea: repasarlo todo.
func repasar(w *watcher) {
	w.mu.Lock()
	w.resync = true
	w.mu.Unlock()
	w.flush()
}

// P3: una sesión que vigila por sondeo releía cada pollInterval, con w.mu
// tomado, todos los directorios vigilados que tienen foto, aunque nada hubiese
// cambiado. 100 directorios de 500 entradas.
func BenchmarkRepasoSondeoSinCambios(b *testing.B) {
	root := b.TempDir()
	var dirs []string
	viejo := time.Now().Add(-time.Hour)
	for i := range 100 {
		d := fmt.Sprintf("d%03d", i)
		p := filepath.Join(root, d)
		if err := os.Mkdir(p, 0o755); err != nil {
			b.Fatal(err)
		}
		for j := range 500 {
			if err := os.WriteFile(filepath.Join(p, fmt.Sprintf("f%04d.js", j)), nil, 0o644); err != nil {
				b.Fatal(err)
			}
		}
		if err := os.Chtimes(p, viejo, viejo); err != nil {
			b.Fatal(err)
		}
		dirs = append(dirs, d)
	}
	w := watcherSondeo(b, root, dirs, func([]byte) { b.Error("a push with nothing changed") })
	b.ResetTimer()
	for range b.N {
		repasar(w)
	}
}

// P3: el repaso no relee la foto de un directorio cuya fecha no cambió desde
// bastante antes de leerla; sí la de uno con fecha reciente, que en un sistema
// de fechas gruesas podría cambiar sin moverla.
func TestRepasoNoReleeFotosQuietas(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"viejo", "reciente"} {
		must(t, os.Mkdir(filepath.Join(root, d), 0o755))
		must(t, os.WriteFile(filepath.Join(root, d, "a"), nil, 0o644))
	}
	viejo := time.Now().Add(-time.Hour)
	must(t, os.Chtimes(filepath.Join(root, "viejo"), viejo, viejo))
	r := &recogedor{t: t}
	w := watcherSondeo(t, root, []string{"viejo", "reciente"}, r.push)
	foto := func(d string) string {
		w.mu.Lock()
		defer w.mu.Unlock()
		return fmt.Sprintf("%p", w.entries[d].snap)
	}
	antesViejo, antesReciente := foto("viejo"), foto("reciente")
	repasar(w)
	if foto("viejo") != antesViejo {
		t.Error("the snapshot of a directory unchanged for an hour was read again")
	}
	if foto("reciente") == antesReciente {
		t.Error("the snapshot of a directory changed just before it was taken was not read again")
	}
	// Y un cambio de verdad se sigue viendo.
	must(t, os.WriteFile(filepath.Join(root, "viejo", "b"), nil, 0o644))
	repasar(w)
	if got := r.espera(1, time.Second); len(got) != 1 || got[0] != "C viejo/b" {
		t.Fatalf("pushes %v, want [C viejo/b]", got)
	}
}

// sinSitio es un backend al que no le caben más vigilancias (los descriptores de
// kqueue del daemon gastados por otras sesiones).
type sinSitio struct{}

func (sinSitio) add(string, bool) error { return errWatchFull }
func (sinSitio) remove(string)          {}
func (sinSitio) close()                 {}

// F8: sin sitio para una vigilancia más del sistema la ruta se olvidaba: los
// vigilantes del invitado no disparaban nunca con sus cambios. Ahora se vigila
// por sondeo.
func TestSinSitioSeVigilaPorSondeo(t *testing.T) {
	viejo := pollInterval
	pollInterval = 20 * time.Millisecond
	t.Cleanup(func() { pollInterval = viejo })
	root := t.TempDir()
	must(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("v1"), 0o644))
	srv, err := Open(root, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	r := &recogedor{t: t}
	w := &watcher{srv: srv, be: sinSitio{}, entries: map[string]*watchEntry{}, lru: list.New(),
		echo: map[string]time.Time{}, pend: map[string]rawEvent{}, push: r.push}
	defer w.close()
	w.note("f.txt", false)
	must(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("version 2"), 0o644))
	if got := r.espera(1, 2*time.Second); len(got) != 1 || got[0] != "M f.txt" {
		t.Fatalf("pushes %v, want [M f.txt]", got)
	}
}

// F8: pasado el tope de vigilancias del sistema de una sesión (1024 en macOS)
// la ruta menos reciente se soltaba en silencio. Ahora pasa a sondeo y sus
// cambios siguen llegando.
func TestPasadoElTopeLaMenosRecienteSigueAvisando(t *testing.T) {
	viejo := pollInterval
	pollInterval = 20 * time.Millisecond
	t.Cleanup(func() { pollInterval = viejo })
	root := t.TempDir()
	const n = 1025
	for i := range n {
		must(t, os.WriteFile(filepath.Join(root, "f"+strconv.Itoa(i)), nil, 0o644))
	}
	srv, err := Open(root, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	r := &recogedor{t: t}
	w, err := newWatcher(srv, r.push)
	if err != nil {
		t.Skipf("no host file watching here: %v", err)
	}
	defer w.close()
	for i := range n {
		w.note("f"+strconv.Itoa(i), false)
	}
	must(t, os.WriteFile(filepath.Join(root, "f0"), []byte("cambio"), 0o644))
	if got := r.espera(1, 2*time.Second); len(got) != 1 || got[0] != "M f0" {
		t.Fatalf("pushes %v, want [M f0]", got)
	}
}
