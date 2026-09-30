//go:build darwin

package custodio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type raiz struct{ snaps, mdir string }

func nuevaRaiz(t *testing.T) raiz {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := raiz{snaps: filepath.Join(root, "snapshots"), mdir: filepath.Join(root, "machines", "m1")}
	for _, d := range []string{r.snaps, r.mdir, filepath.Join(r.snaps, "nuevo"), filepath.Join(r.snaps, "hecho")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Un dorado terminado de otra máquina.
	for f, c := range map[string]string{"mem.file": "memoria buena", "snap.file": "{}", "meta.json": "{}"} {
		if err := os.WriteFile(filepath.Join(r.snaps, "hecho", f), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(r.mdir, "tmp"), []byte("volcado"), 0o600); err != nil {
		t.Fatal(err)
	}
	return r
}

// El caso de kling commit: el volcado del directorio de la máquina acaba en
// el directorio nuevo del dorado, y lo que se escriba después en el origen no
// llega al dorado (es un clon, no un enlace).
func TestPublicaEnUnDoradoNuevo(t *testing.T) {
	r := nuevaRaiz(t)
	dst := filepath.Join(r.snaps, "nuevo", "mem.file")
	if err := Publicar(r.snaps, r.mdir, filepath.Join(r.mdir, "tmp"), dst); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.mdir, "tmp"), []byte("reescrito después"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "volcado" {
		t.Fatalf("el dorado tiene %q", b)
	}
}

// Lo que un kling-vz tomado pediría para tocar lo de otros: nada pasa, y el
// dorado terminado sigue igual.
func TestNoPisaNiSaleDeSuSitio(t *testing.T) {
	r := nuevaRaiz(t)
	tmp := filepath.Join(r.mdir, "tmp")
	if err := os.Symlink(filepath.Join(r.snaps, "hecho"), filepath.Join(r.snaps, "enlace")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(r.snaps, "hecho", "mem.file"), filepath.Join(r.mdir, "enlace")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(r.mdir, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Un directorio sin meta.json pero con el fichero ya puesto.
	if err := os.WriteFile(filepath.Join(r.snaps, "nuevo", "snap.file"), []byte("ya"), 0o600); err != nil {
		t.Fatal(err)
	}
	casos := []struct{ nombre, origen, destino, error string }{
		{"dorado terminado", tmp, filepath.Join(r.snaps, "hecho", "mem.file"), "already finished"},
		{"fichero que ya existe", tmp, filepath.Join(r.snaps, "nuevo", "snap.file"), "already exists"},
		{"otro nombre", tmp, filepath.Join(r.snaps, "nuevo", "overlay.ext4"), "not snapshots/"},
		{"fuera de snapshots", tmp, filepath.Join(filepath.Dir(r.snaps), "secrets", "mem.file"), "not snapshots/"},
		{"anidado", tmp, filepath.Join(r.snaps, "nuevo", "x", "mem.file"), "not snapshots/"},
		{"con ..", tmp, r.snaps + "/nuevo/../hecho/mem.file", "not snapshots/"},
		{"directorio enlace", tmp, filepath.Join(r.snaps, "enlace", "mem.file"), "enlace"},
		{"directorio que no existe", tmp, filepath.Join(r.snaps, "falta", "mem.file"), "daemon creates it"},
		{"origen de fuera", filepath.Join(r.snaps, "hecho", "mem.file"), filepath.Join(r.snaps, "nuevo", "mem.file"), "not a file of this machine"},
		{"origen enlace", filepath.Join(r.mdir, "enlace"), filepath.Join(r.snaps, "nuevo", "mem.file"), "source"},
		{"origen directorio", filepath.Join(r.mdir, "dir"), filepath.Join(r.snaps, "nuevo", "mem.file"), "not a regular file"},
		{"origen con ..", r.mdir + "/../m1/tmp", filepath.Join(r.snaps, "nuevo", "mem.file"), "not a file of this machine"},
	}
	for _, c := range casos {
		err := Publicar(r.snaps, r.mdir, c.origen, c.destino)
		if err == nil || !strings.Contains(err.Error(), c.error) {
			t.Errorf("%s: err = %v, quería %q", c.nombre, err, c.error)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(r.snaps, "hecho", "mem.file")); string(b) != "memoria buena" {
		t.Fatalf("el dorado terminado cambió: %q", b)
	}
	if _, err := os.Stat(filepath.Join(r.snaps, "nuevo", "mem.file")); err == nil {
		t.Fatal("se escribió un mem.file desde un origen rechazado")
	}
}
