package machine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/internal/esquema"
	"github.com/juan52878911/kindling/internal/events"
)

func modo(t *testing.T, ruta string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(ruta)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// M8: state.json y fondos.json son el inventario (IPs, reenvíos, dueños,
// nombres de variables) y los escribía 0644: legibles por cualquier cuenta.
func TestEstadoYFondosSoloLosLeeElDaemon(t *testing.T) {
	var permisos []os.FileMode
	prevE, prevF := escribirEstado, escribirFondosEn
	espia := func(prev func(string, []byte, os.FileMode) error) func(string, []byte, os.FileMode) error {
		return func(ruta string, datos []byte, perm os.FileMode) error {
			permisos = append(permisos, perm)
			return prev(ruta, datos, perm)
		}
	}
	escribirEstado, escribirFondosEn = espia(prevE), espia(prevF)
	t.Cleanup(func() { escribirEstado, escribirFondosEn = prevE, prevF })

	m := newTestManager(t)
	m.addForTest("e8e8000000000001")
	if err := m.persistirYa(); err != nil {
		t.Fatal(err)
	}
	if got := modo(t, m.statePath()); got != 0o600 {
		t.Errorf("state.json %v; quería 0600", got)
	}
	if err := m.guardarFondos(); err != nil {
		t.Fatal(err)
	}
	if got := modo(t, m.fondosPath()); got != 0o600 {
		t.Errorf("fondos.json %v; quería 0600", got)
	}
	if len(permisos) < 2 {
		t.Fatalf("solo %d escrituras vistas; quería state.json y fondos.json", len(permisos))
	}
	for _, p := range permisos {
		if p != 0o600 {
			t.Errorf("se escribió con %v; quería 0600", p)
		}
	}
}

// Firecracker crea mem.file y snap.file 0644: cualquier cuenta del host leía
// la RAM de una máquina congelada. cerrarVolcado les quita la lectura ajena.
func TestCerrarVolcadoQuitaLaLecturaAjena(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"mem.file", "snap.file"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("ram"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cerrarVolcado(dir)
	for _, f := range []string{"mem.file", "snap.file"} {
		if m := modo(t, filepath.Join(dir, f)); m&0o007 != 0 {
			t.Errorf("%s = %o, quería sin permisos para otros", f, m)
		}
	}
}

// Los volcados que ya estaban en disco (de versiones anteriores) se cierran al
// arrancar, y los directorios de datos dejan de ser 0755.
func TestArranqueCierraDirectoriosYVolcadosExistentes(t *testing.T) {
	root := t.TempDir()
	maq := filepath.Join(root, "machines", "abc")
	if err := os.MkdirAll(maq, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"snapshots", "volumes", "images", "jails"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mem := filepath.Join(maq, "mem.file")
	if err := os.WriteFile(mem, []byte("ram"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	restringirRaiz(root, &Privileges{})
	cerrarVolcadosExistentes(root)

	if m := modo(t, root); m != 0o750 {
		t.Errorf("raíz = %o, quería 0750", m)
	}

	for _, d := range []string{"machines", "snapshots", "volumes", "images", "jails"} {
		if m := modo(t, filepath.Join(root, d)); m&0o007 != 0 {
			t.Errorf("%s = %o, quería cerrado a los demás", d, m)
		}
	}
	if m := modo(t, mem); m&0o007 != 0 {
		t.Errorf("mem.file existente = %o, quería cerrado a los demás", m)
	}
}

// EnsureReadable no sigue enlaces simbólicos: uno plantado dentro del árbol
// (por un VMM comprometido, en un directorio suyo) no puede llevar el chmod o
// el chown a un fichero de fuera.
func TestEnsureReadableNoSigueEnlaces(t *testing.T) {
	dentro, fuera := t.TempDir(), t.TempDir()
	victima := filepath.Join(fuera, "shadow")
	if err := os.WriteFile(victima, []byte("x"), 0o604); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victima, filepath.Join(dentro, "mem.file")); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(dentro, "min.ext4")
	if err := os.WriteFile(img, []byte("base"), 0o666); err != nil {
		t.Fatal(err)
	}
	p := &Privileges{Enabled: true, UID: os.Getuid(), GID: os.Getgid()}
	p.EnsureReadable(dentro)

	if m := modo(t, victima); m != 0o604 {
		t.Errorf("el fichero de fuera cambió de modo (%o): se siguió el enlace", m)
	}
	if m := modo(t, img); m != 0o640 {
		t.Errorf("la imagen = %o, quería 0640", m)
	}
}

// La receta puede llevar secretos y el VMM no la lee: queda 0600.
func TestEnsureReadableCierraLasRecetas(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "svc.recipe.json")
	if err := os.WriteFile(rec, []byte(`{"env":{"TOKEN":"x"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &Privileges{Enabled: true, UID: os.Getuid(), GID: os.Getgid()}
	p.EnsureReadable(dir)
	if m := modo(t, rec); m != 0o600 {
		t.Errorf("receta = %o, quería 0600", m)
	}
}

// darLecturaVMM no toca lo que ya es del VMM (su overlay, sus volúmenes): ni
// dueño ni modo.
func TestDarLecturaVMMRespetaLoSuyo(t *testing.T) {
	f := filepath.Join(t.TempDir(), "overlay.ext4")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	darLecturaVMM(f, &Privileges{Enabled: true, UID: os.Getuid(), GID: os.Getgid()})
	if m := modo(t, f); m != 0o600 {
		t.Errorf("overlay del VMM = %o, no debía cambiar", m)
	}
}

// El respaldo .vN.bak de un state.json de una instalación vieja (0644) es el
// mismo inventario que state.json: 0600, no el modo del original.
func TestRespaldoDelEstadoSoloLoLeeElDaemon(t *testing.T) {
	m := newTestManager(t)
	if err := os.WriteFile(m.statePath(), []byte(`[]`), 0o644); err != nil {
		t.Fatal(err)
	}
	m.load()
	bak := esquema.RutaRespaldo(m.statePath(), 0)
	if got := modo(t, bak); got != 0o600 {
		t.Errorf("%s = %v; quería 0600", filepath.Base(bak), got)
	}
}

// Una raíz de otro dueño que el daemon no se toca: ni modo ni grupo.
func TestRaizAjenaNoSeToca(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	cerrarRaiz(root, &Privileges{}, os.Geteuid()+1)
	if m := modo(t, root); m != 0o755 {
		t.Errorf("raíz ajena = %o, quería 0755 intacta", m)
	}
	cerrarRaiz(root, &Privileges{}, os.Geteuid())
	if m := modo(t, root); m != 0o750 {
		t.Errorf("raíz propia = %o, quería 0750", m)
	}
}

// NewManager crea la raíz, machines/ e images/ cerradas a los demás.
func TestNewManagerCreaLosDirectoriosCerrados(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kindling")
	m, err := NewManager(root, "firecracker", "", events.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	for _, d := range []string{root, filepath.Join(root, "machines"), filepath.Join(root, "images")} {
		if got := modo(t, d); got&0o007 != 0 {
			t.Errorf("%s = %o, quería cerrado a los demás", d, got)
		}
	}
}
