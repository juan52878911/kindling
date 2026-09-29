//go:build !darwin

package machine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// FICLONE clona o falla limpio (sin dejar el destino), según el sistema de
// ficheros del directorio temporal; y copiarOverlay en modo reflink cae a la
// copia de siempre cuando no puede clonar.
func TestClonarFicheroOFallaLimpio(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(src, []byte("contenido"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := clonarFichero(src, dst); err != nil {
		if _, serr := os.Lstat(dst); !os.IsNotExist(serr) {
			t.Errorf("un FICLONE fallido (%v) dejó el destino: %v", err, serr)
		}
		if probarReflink(dir, dir) == nil {
			t.Error("la prueba dice que hay reflink y el clon falló")
		}
	} else if b, _ := os.ReadFile(dst); string(b) != "contenido" {
		t.Errorf("clon mal: %q", b)
	}

	m := newTestManager(t)
	m.cow.modo = cowModoReflink
	dst2 := filepath.Join(dir, "c")
	if out, err := m.copiarOverlay(context.Background(), src, dst2); err != nil {
		t.Fatalf("copiarOverlay: %v: %s", err, out)
	}
	if b, _ := os.ReadFile(dst2); string(b) != "contenido" {
		t.Errorf("copiarOverlay: %q", b)
	}
}

// Sin bind del almacén, borrarJail es el RemoveAll de siempre.
func TestBorrarJailSinBind(t *testing.T) {
	m := newTestManager(t)
	m.alm = nuevoAlmacen(m.root, &Privileges{})
	base := filepath.Join(m.jailBase(), "firecracker", "id1")
	if err := os.MkdirAll(filepath.Join(m.jailRoot("id1"), "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	// El directorio del bind existe pero no hay nada montado encima.
	if err := os.MkdirAll(m.dirBindJail("id1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.borrarJail("id1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(base); !os.IsNotExist(err) {
		t.Errorf("el jail sigue: %v", err)
	}
	// Una instancia sin overlay en el almacén no monta nada en su jail.
	if err := m.prepararBindsJail("id2"); err != nil {
		t.Errorf("prepararBindsJail sin almacén: %v", err)
	}
}

// El bind de verdad (root y KLING_TEST_MOUNTS=1; en un contenedor
// --privileged vale): el overlay se ve dentro del jail, y borrar el jail
// desmonta antes de borrar y NO se lleva el overlay del almacén.
func TestBindDelAlmacenEnElJail(t *testing.T) {
	if os.Geteuid() != 0 || os.Getenv("KLING_TEST_MOUNTS") != "1" {
		t.Skip("needs root and KLING_TEST_MOUNTS=1")
	}
	m := newTestManager(t)
	m.alm = nuevoAlmacen(m.root, &Privileges{})
	// Un tmpfs hace de almacén: lo que importa es que es otro dispositivo.
	if err := os.MkdirAll(m.alm.dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mount("tmpfs", m.alm.dir, "tmpfs", 0, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Unmount(m.alm.dir, syscall.MNT_DETACH) })
	d := m.alm.dirInstancia("id1")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(d, "overlay.ext4")
	if err := os.WriteFile(overlay, []byte("disco"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.prepararBindsJail("id1"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(m.jailRoot("id1"), overlay)); err != nil || string(b) != "disco" {
		t.Fatalf("dentro del jail: %q %v", b, err)
	}
	if err := m.borrarJail("id1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(m.jailBase(), "firecracker", "id1")); !os.IsNotExist(err) {
		t.Errorf("el jail sigue: %v", err)
	}
	if b, err := os.ReadFile(overlay); err != nil || string(b) != "disco" {
		t.Fatalf("borrar el jail se llevó el overlay del almacén: %q %v", b, err)
	}
}

// Un almacén existente conserva su tipo: el fichero de imagen lo dice.
func TestFSDelAlmacenExistente(t *testing.T) {
	for _, c := range []struct{ img, fs string }{{"cow.xfs", "xfs"}, {"cow.btrfs", "btrfs"}} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, c.img), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		fs, err := fsDelAlmacen(root)
		if err != nil || fs != c.fs {
			t.Errorf("%s: %q %v", c.img, fs, err)
		}
		a := nuevoAlmacen(root, &Privileges{})
		if a.fs != c.fs || filepath.Base(a.img) != c.img || !a.existe() {
			t.Errorf("%s: almacén %s en %s", c.img, a.fs, a.img)
		}
	}
}

// Btrfs se formatea sin discard, con datos y metadatos single, y se monta sin
// discard: un discard agujerearía el fichero reservado. Los dos, sin
// dispositivos, suid ni ejecutables.
func TestArgsYOpcionesBtrfs(t *testing.T) {
	args := strings.Join(argsMkfs("btrfs"), " ")
	for _, w := range []string{"-K", "-m single", "-d single", "-L kling-cow"} {
		if !strings.Contains(args, w) {
			t.Errorf("mkfs.btrfs sin %q: %s", w, args)
		}
	}
	if args := strings.Join(argsMkfs("xfs"), " "); !strings.Contains(args, "reflink=1") {
		t.Errorf("mkfs.xfs sin reflink: %s", args)
	}
	for _, fs := range []string{"xfs", "btrfs"} {
		o := opcionesMontaje(fs)
		for _, w := range []string{"loop", "nodev", "nosuid", "noexec"} {
			if !strings.Contains(o, w) {
				t.Errorf("%s: montaje sin %s: %s", fs, w, o)
			}
		}
	}
	if !strings.Contains(opcionesMontaje("btrfs"), "nodiscard") {
		t.Error("btrfs se monta con discard")
	}
}

// El almacén Btrfs de verdad (root, KLING_TEST_MOUNTS=1, btrfs en el núcleo y
// btrfs-progs): se crea reservado, se monta con sus opciones y clona
// instancias de verdad desde la base.
func TestAlmacenBtrfsDeVerdad(t *testing.T) {
	if os.Geteuid() != 0 || os.Getenv("KLING_TEST_MOUNTS") != "1" {
		t.Skip("needs root and KLING_TEST_MOUNTS=1")
	}
	if b, _ := os.ReadFile("/proc/filesystems"); !soportados(string(b))["btrfs"] || buscarE2fs("mkfs.btrfs") == "" {
		t.Skip("needs btrfs in the kernel and mkfs.btrfs")
	}
	root := t.TempDir()
	a := nuevoAlmacen(root, &Privileges{})
	// Forzado a Btrfs aunque el núcleo tenga XFS: es lo que se prueba.
	a.fs, a.img, a.errFS = "btrfs", filepath.Join(a.root, "cow.btrfs"), nil
	a.estaMontado = func(dir string) (bool, error) { return estaMontadoTipo(dir, "btrfs") }
	a.crear = func(ctx context.Context, img string, bytes int64) error {
		return crearImagenAlmacen(ctx, "btrfs", img, bytes)
	}
	a.montar = func(ctx context.Context, img, dir string) error {
		return montarLoopAlmacen(ctx, "btrfs", img, dir)
	}
	t.Cleanup(func() { _ = syscall.Unmount(a.dir, syscall.MNT_DETACH) })

	src := filepath.Join(root, "dorado.ext4")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(512 << 20); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("disco de verdad"), 100<<20); err != nil {
		t.Fatal(err)
	}
	f.Close()

	ruta, err := a.clonarInstancia(context.Background(), "d", src, "id1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := estaMontadoTipo(a.dir, "btrfs"); !ok || err != nil {
		t.Fatalf("montado como btrfs: %v %v", ok, err)
	}
	// Reservado entero: ni mkfs ni el montaje lo agujerearon.
	var st syscall.Stat_t
	if err := syscall.Stat(a.img, &st); err != nil {
		t.Fatal(err)
	}
	if ocupa := int64(st.Blocks) * 512; ocupa < st.Size {
		t.Errorf("la imagen ocupa %d de %d MiB: no está reservada entera", ocupa>>20, st.Size>>20)
	}
	g, err := os.Open(ruta)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	b := make([]byte, 15)
	if _, err := g.ReadAt(b, 100<<20); err != nil || string(b) != "disco de verdad" {
		t.Errorf("la instancia lee %q %v", b, err)
	}
	// Una segunda instancia de la misma base, también por FICLONE.
	if _, err := a.clonarInstancia(context.Background(), "d", src, "id2", 1); err != nil {
		t.Fatal(err)
	}
	if s := a.info(); s == nil || !s.Mounted || s.FS != "btrfs" || s.SizeMiB == 0 {
		t.Errorf("info: %+v", s)
	}
	mi, _ := os.ReadFile("/proc/self/mountinfo")
	for _, l := range strings.Split(string(mi), "\n") {
		if strings.Contains(l, " "+a.dir+" ") && strings.Contains(l, "discard") && !strings.Contains(l, "nodiscard") {
			t.Errorf("montado con discard: %s", l)
		}
	}
}
