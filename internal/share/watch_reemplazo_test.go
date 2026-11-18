package share

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// F19: tras un guardado atómico (escribir aparte y renombrar encima) la
// vigilancia seguía en el inodo viejo. Con el viejo aún abierto el sistema no
// avisa de su borrado, solo de un cambio de atributos: se avisaba de ese
// cambio, y de los siguientes del fichero nuevo ya no se enteraba nadie.
func TestGuardadoAtomicoSigueVigilandoElFicheroNuevo(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f.txt")
	must(t, os.WriteFile(f, []byte("v1"), 0o644))
	srv, err := Open(dir, false, "")
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
	if w.polling {
		t.Skip("polling backend: it does not follow inodes")
	}
	w.note("f.txt", false)

	// Alguien (un lector del host, otro handle) tiene abierto el viejo.
	viejo, err := os.Open(f)
	if err != nil {
		t.Fatal(err)
	}
	defer viejo.Close()

	tmp := filepath.Join(dir, ".f.txt.tmp")
	must(t, os.WriteFile(tmp, []byte("version 2"), 0o644))
	must(t, os.Rename(tmp, f))
	if got := r.espera(1, 2*time.Second); len(got) != 1 || got[0] != "M f.txt" {
		t.Fatalf("after the atomic save, pushes %v, want [M f.txt]", got)
	}
	time.Sleep(5 * watchDebounce)

	must(t, os.WriteFile(f, []byte("version 3, longer"), 0o644))
	if got := r.espera(2, 2*time.Second); len(got) != 2 || got[1] != "M f.txt" {
		t.Fatalf("a change to the new file after the atomic save: pushes %v, want [M f.txt M f.txt]", got)
	}
}
