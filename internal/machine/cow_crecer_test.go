package machine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// #61: si el tipo elegido no monta (el núcleo no tiene el módulo), su imagen
// se desmonta y se borra y se prueba con el otro: no queda atrás un fichero
// reservado que no sirve.
func TestAlmacenQueNoMontaPruebaElOtroTipo(t *testing.T) {
	root := t.TempDir()
	f := &almacenFalso{}
	a := nuevoAlmacenFalso(t, root, f)
	montarFalso := a.montar
	a.montar = func(ctx context.Context, img, dir string) error {
		if a.fs == "btrfs" {
			return errors.New("mount: unknown filesystem type 'btrfs'")
		}
		return montarFalso(ctx, img, dir)
	}
	a.candidatos = func() []string { return []string{"btrfs", "xfs"} }
	btrfs := a.img
	src := escribirDorado(t, root, "d", "disco")
	ruta, err := a.clonarInstancia(context.Background(), "d", src, "id1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(ruta); string(b) != "disco" {
		t.Errorf("contenido %q", b)
	}
	if _, err := os.Lstat(btrfs); !os.IsNotExist(err) {
		t.Errorf("la imagen Btrfs que no montó sigue ahí: %v", err)
	}
	if a.fs != "xfs" || filepath.Base(a.img) != "cow.xfs" || !a.existe() {
		t.Errorf("almacén %s en %s", a.fs, a.img)
	}
	if s := a.info(); s == nil || s.FS != "xfs" || !s.Mounted {
		t.Errorf("info: %+v", s)
	}
	if f.creados.Load() != 2 {
		t.Errorf("creado %d veces, quería 2", f.creados.Load())
	}
}

// Si ninguno sirve, no queda ninguna imagen, el error dice por qué con cada
// uno, y no se reintenta en cada instancia.
func TestAlmacenQueNoSirveNoDejaImagen(t *testing.T) {
	root := t.TempDir()
	f := &almacenFalso{falloMontar: errors.New("mount: unknown filesystem type")}
	a := nuevoAlmacenFalso(t, root, f)
	a.candidatos = func() []string { return []string{"xfs", "btrfs"} }
	src := escribirDorado(t, root, "d", "x")
	_, err := a.clonarInstancia(context.Background(), "d", src, "id1", 0)
	if !errors.Is(err, errAlmacenNoDisponible) {
		t.Fatalf("quería errAlmacenNoDisponible, dio %v", err)
	}
	for _, w := range []string{"btrfs: ", "xfs: ", "the kernel has no btrfs"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("el error no dice %q: %v", w, err)
		}
	}
	for _, img := range []string{"cow.xfs", "cow.btrfs"} {
		if _, err := os.Lstat(filepath.Join(root, img)); !os.IsNotExist(err) {
			t.Errorf("%s se quedó: %v", img, err)
		}
	}
	if _, err := a.clonarInstancia(context.Background(), "d", src, "id2", 0); !errors.Is(err, errAlmacenNoDisponible) {
		t.Fatal(err)
	}
	if f.creados.Load() != 2 {
		t.Errorf("creado %d veces, quería 2 (una por tipo, y sin reintentar)", f.creados.Load())
	}
}

// Monta pero no clona: se desmonta antes de borrar la imagen.
func TestAlmacenQueNoClonaSeDesmontaYSeBorra(t *testing.T) {
	root := t.TempDir()
	f := &almacenFalso{falloClonar: errors.New("EOPNOTSUPP")}
	a := nuevoAlmacenFalso(t, root, f)
	src := escribirDorado(t, root, "d", "x")
	if _, err := a.clonarInstancia(context.Background(), "d", src, "id1", 0); !errors.Is(err, errAlmacenNoDisponible) {
		t.Fatal(err)
	}
	if f.desmontajes.Load() != 1 || f.montado {
		t.Errorf("desmontajes=%d montado=%v", f.desmontajes.Load(), f.montado)
	}
	if a.existe() {
		t.Error("la imagen se quedó")
	}
	// Si no se puede desmontar, no se borra: con la imagen montada, borrarla
	// no liberaría nada.
	root2 := t.TempDir()
	f2 := &almacenFalso{falloClonar: errors.New("EOPNOTSUPP")}
	a2 := nuevoAlmacenFalso(t, root2, f2)
	a2.desmontar = func(string) error { return errors.New("device busy") }
	src2 := escribirDorado(t, root2, "d", "x")
	_, err := a2.clonarInstancia(context.Background(), "d", src2, "id1", 0)
	if err == nil || !strings.Contains(err.Error(), "could not be removed") {
		t.Errorf("error: %v", err)
	}
	if !a2.existe() {
		t.Error("borró una imagen que sigue montada")
	}
}

// Al arrancar: un almacén que no monta y que ninguna máquina usa se quita (el
// primer run -from lo vuelve a crear); si alguna lo usa, se queda.
func TestMontarSiExisteQuitaUnAlmacenSinUso(t *testing.T) {
	for _, conMaquina := range []bool{false, true} {
		root := t.TempDir()
		f := &almacenFalso{falloMontar: errors.New("mount: unknown filesystem type 'btrfs'")}
		a := nuevoAlmacenFalso(t, root, f)
		a.candidatos = func() []string { return []string{"xfs"} }
		if err := os.WriteFile(a.img, []byte("imagen"), 0o600); err != nil {
			t.Fatal(err)
		}
		img := a.img
		dm := filepath.Join(root, "machines", "id1")
		if err := os.MkdirAll(dm, 0o755); err != nil {
			t.Fatal(err)
		}
		destino := filepath.Join(root, "otro-sitio", "overlay.ext4")
		if conMaquina {
			destino = filepath.Join(a.dirInstancia("id1"), "overlay.ext4")
		}
		if err := os.Symlink(destino, filepath.Join(dm, "overlay.ext4")); err != nil {
			t.Fatal(err)
		}
		a.montarSiExiste(context.Background())
		_, err := os.Lstat(img)
		switch {
		case conMaquina && err != nil:
			t.Errorf("borró un almacén que usa una máquina: %v", err)
		case !conMaquina && !os.IsNotExist(err):
			t.Errorf("un almacén sin uso que no monta se quedó: %v", err)
		case !conMaquina && a.fs != "xfs":
			t.Errorf("el almacén nuevo sería %s, quería el primer candidato", a.fs)
		}
	}
}

// Al arrancar, un almacén sin uso que no monta solo se borra si el fallo es
// definitivo (sin el sistema de ficheros, o sin permiso para montar); con
// cualquier otro, que puede ser pasajero, se avisa y la imagen se queda.
func TestMontarSiExisteSoloBorraConFalloDefinitivo(t *testing.T) {
	casos := []struct {
		err    string
		borrar bool
	}{
		{"mount: unknown filesystem type 'xfs'", true},
		{"mount: /var/lib/kindling/cow: permission denied", true},
		{"mount: operation not permitted", true},
		{"mount: /dev/loop3: can't read superblock", false},
		{"losetup: /var/lib/kindling/cow.xfs: failed to set up loop device: Device or resource busy", false},
		{"context deadline exceeded", false},
	}
	for _, c := range casos {
		root := t.TempDir()
		a := nuevoAlmacenFalso(t, root, &almacenFalso{falloMontar: errors.New(c.err)})
		if err := os.WriteFile(a.img, []byte("imagen"), 0o600); err != nil {
			t.Fatal(err)
		}
		img := a.img
		a.montarSiExiste(context.Background())
		_, err := os.Lstat(img)
		switch {
		case c.borrar && !os.IsNotExist(err):
			t.Errorf("%q: definitive failure, but the image stayed (%v)", c.err, err)
		case !c.borrar && err != nil:
			t.Errorf("%q: possibly transient failure, but the image was removed (%v)", c.err, err)
		}
	}
}

func TestOrdenCandidatos(t *testing.T) {
	hay := func(bins ...string) func(string) bool {
		return func(b string) bool { return slices.Contains(bins, b) }
	}
	casos := []struct {
		fs     string
		mkfs   func(string) bool
		quiero []string
	}{
		{"\txfs\n\tbtrfs\n", hay("mkfs.xfs", "mkfs.btrfs"), []string{"xfs", "btrfs"}},
		// El LXC del laboratorio: sin xfs en el núcleo, btrfs primero.
		{"\tbtrfs\n", hay("mkfs.xfs", "mkfs.btrfs"), []string{"btrfs", "xfs"}},
		{"\text4\n", hay("mkfs.btrfs"), []string{"btrfs"}},
		{"\txfs\n", hay(), nil},
	}
	for _, c := range casos {
		if got := ordenCandidatos(c.fs, c.mkfs); !slices.Equal(got, c.quiero) {
			t.Errorf("%q: %v, quería %v", c.fs, got, c.quiero)
		}
	}
}

// #61: crecer amplía el almacén montado; no encoge, no se come el margen de
// la raíz y "+N" suma a lo que tiene.
func TestAlmacenCrecer(t *testing.T) {
	root := t.TempDir()
	f := &almacenFalso{libre: 20 << 30}
	a := nuevoAlmacenFalso(t, root, f)
	var pedido int64
	a.agrandar = func(_ context.Context, img, _ string, bytes int64) error {
		pedido = bytes
		return os.Truncate(img, bytes)
	}
	ctx := context.Background()
	if err := a.crecer(ctx, 8<<30, 0); err == nil || !strings.Contains(err.Error(), "created on the first run -from") {
		t.Errorf("sin almacén: %v", err)
	}
	if err := os.WriteFile(a.img, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.crecer(ctx, 8<<30, 0); err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Errorf("sin montar: %v", err)
	}
	f.montado = true
	a.mu.Lock()
	if err := a.asegurarMontado(ctx); err != nil {
		t.Fatal(err)
	}
	a.mu.Unlock()
	if err := os.Truncate(a.img, 4<<30); err != nil {
		t.Fatal(err)
	}
	if err := a.crecer(ctx, 2<<30, 0); err == nil || !strings.Contains(err.Error(), "can't shrink") {
		t.Errorf("encoger: %v", err)
	}
	if err := a.crecer(ctx, 0, 19<<30); err == nil || !strings.Contains(err.Error(), "doesn't fit") {
		t.Errorf("sin margen en la raíz: %v", err)
	}
	if err := a.crecer(ctx, 0, 8<<30); err != nil || pedido != 12<<30 {
		t.Errorf("+8G: %v, pidió %d MiB", err, pedido>>20)
	}
	// El mismo tamaño repite el loop y el sistema de ficheros (completar uno
	// a medias) sin pedir más sitio.
	pedido = 0
	if err := a.crecer(ctx, 12<<30, 0); err != nil || pedido != 12<<30 {
		t.Errorf("mismo tamaño: %v, pidió %d MiB", err, pedido>>20)
	}
}

func TestGrowCoWStoreValida(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.GrowCoWStore(context.Background(), 1024, 0); err == nil {
		t.Error("sin almacén en la plataforma")
	}
	m.alm = nuevoAlmacenFalso(t, m.root, &almacenFalso{})
	for _, c := range [][2]int64{{0, 0}, {1024, 1024}, {-1, 0}, {0, -5}, {1 << 40, 0}} {
		if _, err := m.GrowCoWStore(context.Background(), c[0], c[1]); err == nil {
			t.Errorf("%v: aceptado", c)
		}
	}
}

func TestNombreLoopYCrecerFS(t *testing.T) {
	for _, c := range []struct {
		fuente, nombre string
		ok             bool
	}{
		{"/dev/loop3", "loop3", true},
		{"/dev/loop12", "loop12", true},
		{"/dev/loop", "", false},
		{"/dev/sda1", "", false},
		{"/dev/loop3p1", "", false},
		{"/dev/../dev/loop3", "", false},
		{"loop3", "", false},
	} {
		n, ok := nombreLoop(c.fuente)
		if n != c.nombre || ok != c.ok {
			t.Errorf("%q: %q %v", c.fuente, n, ok)
		}
	}
	if bin, args := argsCrecerFS("xfs", "/r/cow"); bin != "xfs_growfs" || !slices.Equal(args, []string{"/r/cow"}) {
		t.Errorf("xfs: %s %v", bin, args)
	}
	if bin, args := argsCrecerFS("btrfs", "/r/cow"); bin != "btrfs" || strings.Join(args, " ") != "filesystem resize max /r/cow" {
		t.Errorf("btrfs: %s %v", bin, args)
	}
}

// La fuente del montaje (el loop) sale de mountinfo.
func TestParsearMountinfoFuente(t *testing.T) {
	ms, err := parsearMountinfo(strings.NewReader("40 22 7:0 / /var/lib/kindling/cow rw,nodev shared:20 - xfs /dev/loop0 rw,attr2\n"))
	if err != nil || len(ms) != 1 || ms[0].fuente != "/dev/loop0" {
		t.Errorf("%+v %v", ms, err)
	}
}
