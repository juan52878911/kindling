package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestDecidirCoW(t *testing.T) {
	sinXFS := errors.New("mkfs.xfs not found")
	casos := []struct {
		pedido string
		nativo bool
		errAlm error
		modo   string
	}{
		{CoWOff, true, nil, cowModoCopy},
		{CoWAuto, true, nil, cowModoReflink},
		{"", true, sinXFS, cowModoReflink},
		{CoWAuto, false, nil, cowModoStore},
		{CoWAuto, false, sinXFS, cowModoCopy},
		// reflink-store fuerza el almacén aunque la raíz clone.
		{CoWStore, true, nil, cowModoStore},
		{CoWStore, false, sinXFS, cowModoCopy},
		{"quizas", true, nil, cowModoCopy},
	}
	for _, c := range casos {
		modo, motivo := decidirCoW(c.pedido, c.nativo, c.errAlm)
		if modo != c.modo {
			t.Errorf("decidirCoW(%q, %v, %v) = %q, quería %q", c.pedido, c.nativo, c.errAlm, modo, c.modo)
		}
		if motivo == "" {
			t.Errorf("decidirCoW(%q) sin motivo", c.pedido)
		}
		if modo == cowModoCopy && c.errAlm != nil && !strings.Contains(motivo, c.errAlm.Error()) {
			t.Errorf("el motivo de la copia no dice por qué: %q", motivo)
		}
	}
}

func TestTamAlmacen(t *testing.T) {
	const gib = int64(1) << 30
	if b, err := tamAlmacen(100*gib, 0); err != nil || b != 16*gib {
		t.Errorf("100 GiB libres: %d %v, quería el tope de 16 GiB", b>>30, err)
	}
	if b, err := tamAlmacen(20*gib, 0); err != nil || b != 5*gib {
		t.Errorf("20 GiB libres: %d %v, quería 5 GiB", b>>30, err)
	}
	if _, err := tamAlmacen(3*gib, 0); err == nil {
		t.Error("3 GiB libres no dan para un almacén")
	}
	if b, err := tamAlmacen(100*gib, 40); err != nil || b != 40*gib {
		t.Errorf("pedido 40: %d %v", b>>30, err)
	}
	if _, err := tamAlmacen(10*gib, 9); err == nil {
		t.Error("pedir 9 GiB con 10 libres deja la raíz sin margen")
	}
}

func TestParsearMountinfo(t *testing.T) {
	in := "22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n" +
		"40 22 7:0 / /var/lib/kindling/cow rw,nodev shared:20 - xfs /dev/loop0 rw,attr2\n" +
		"41 22 0:5 / /mnt/con\\040espacio rw - tmpfs tmpfs rw\n" +
		"basura\n"
	ms, err := parsearMountinfo(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 {
		t.Fatalf("montajes: %+v", ms)
	}
	if ms[1].punto != "/var/lib/kindling/cow" || ms[1].fstype != "xfs" {
		t.Errorf("almacén: %+v", ms[1])
	}
	if ms[2].punto != "/mnt/con espacio" || ms[2].fstype != "tmpfs" {
		t.Errorf("escape octal: %+v", ms[2])
	}
}

func TestNombreSeguro(t *testing.T) {
	for _, n := range []string{"", ".", "..", "../x", "a/b", ".tmp-x", "a\\b"} {
		if nombreSeguro(n) == nil {
			t.Errorf("%q no debería valer", n)
		}
	}
	for _, n := range []string{"mi-dorado", "0123abcd", "sql_v2"} {
		if err := nombreSeguro(n); err != nil {
			t.Errorf("%q: %v", n, err)
		}
	}
}

// almacenFalso es un almacén cuyas operaciones de sistema son de mentira:
// "montar" crea el directorio, "clonar" copia. Cuenta cuántas veces se crea,
// se monta y se copia entero.
type almacenFalso struct {
	creados, montajes, copias atomic.Int32
	libre                     int64
	falloMontar               error
	falloClonar               error
	montado                   bool
}

func nuevoAlmacenFalso(t *testing.T, root string, f *almacenFalso) *almacenCoW {
	t.Helper()
	copiar := func(src, dst string) error {
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		return err
	}
	return &almacenCoW{
		root: root, img: filepath.Join(root, "cow.xfs"), dir: filepath.Join(root, "cow"),
		estaMontado: func(string) (bool, error) { return f.montado, nil },
		crear: func(_ context.Context, img string, _ int64) error {
			f.creados.Add(1)
			return os.WriteFile(img, nil, 0o600)
		},
		montar: func(_ context.Context, _, dir string) error {
			if f.falloMontar != nil {
				return f.falloMontar
			}
			f.montajes.Add(1)
			f.montado = true
			return os.MkdirAll(dir, 0o750)
		},
		clonar: func(src, dst string) error {
			if f.falloClonar != nil {
				return f.falloClonar
			}
			return copiar(src, dst)
		},
		copiar: func(_ context.Context, src, dst string) error {
			f.copias.Add(1)
			return copiar(src, dst)
		},
		libreEn: func(string) (int64, int64, error) {
			if f.libre == 0 {
				return 100 << 30, 100 << 30, nil
			}
			return f.libre, f.libre, nil
		},
	}
}

func escribirDorado(t *testing.T, root, snap, contenido string) string {
	t.Helper()
	dir := filepath.Join(root, "snapshots", snap)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ruta := filepath.Join(dir, "overlay.ext4")
	// Reemplazo como hace commit: otro fichero, otro inodo.
	tmp := ruta + ".nuevo"
	if err := os.WriteFile(tmp, []byte(contenido), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, ruta); err != nil {
		t.Fatal(err)
	}
	return ruta
}

// La base se copia UNA vez por dorado; cada instancia es un clon suyo con su
// propio fichero, en m/<id>.
func TestAlmacenClonaDeUnaBase(t *testing.T) {
	root := t.TempDir()
	f := &almacenFalso{}
	a := nuevoAlmacenFalso(t, root, f)
	src := escribirDorado(t, root, "dorado", "disco v1")

	var rutas []string
	for i := range 3 {
		id := fmt.Sprintf("id%d", i)
		ruta, err := a.clonarInstancia(context.Background(), "dorado", src, id, 0)
		if err != nil {
			t.Fatalf("instancia %d: %v", i, err)
		}
		if ruta != filepath.Join(root, "cow", "m", id, "overlay.ext4") {
			t.Errorf("ruta %q", ruta)
		}
		if b, _ := os.ReadFile(ruta); string(b) != "disco v1" {
			t.Errorf("contenido %q", b)
		}
		rutas = append(rutas, ruta)
	}
	if f.creados.Load() != 1 || f.montajes.Load() != 1 || f.copias.Load() != 1 {
		t.Errorf("creados=%d montajes=%d copias=%d: quería 1/1/1", f.creados.Load(), f.montajes.Load(), f.copias.Load())
	}
	// Independientes: escribir en una no toca a las demás ni a la base.
	if err := os.WriteFile(rutas[0], []byte("cambiado"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(rutas[1]); string(b) != "disco v1" {
		t.Errorf("la instancia 1 vio la escritura de la 0: %q", b)
	}
	if fi, err := os.Stat(rutas[1]); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("modo del clon: %v %v", fi, err)
	}

	// Dorado reemplazado: base nueva, la vieja se va.
	time.Sleep(10 * time.Millisecond)
	src = escribirDorado(t, root, "dorado", "disco v2")
	ruta, err := a.clonarInstancia(context.Background(), "dorado", src, "id9", 0)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(ruta); string(b) != "disco v2" {
		t.Errorf("tras reemplazar el dorado: %q", b)
	}
	bases, _ := os.ReadDir(filepath.Join(root, "cow", "bases", "dorado"))
	if len(bases) != 1 || f.copias.Load() != 2 {
		t.Errorf("bases=%d copias=%d: quería 1 base vigente y 2 copias", len(bases), f.copias.Load())
	}
}

// Si el almacén no se puede preparar, el error lo dice (errAlmacenNoDisponible)
// y no se reintenta en cada instancia.
func TestAlmacenQueNoMontaNoSeReintenta(t *testing.T) {
	root := t.TempDir()
	f := &almacenFalso{falloMontar: errors.New("unknown filesystem type 'xfs'")}
	a := nuevoAlmacenFalso(t, root, f)
	src := escribirDorado(t, root, "d", "x")
	for range 2 {
		if _, err := a.clonarInstancia(context.Background(), "d", src, "id1", 0); !errors.Is(err, errAlmacenNoDisponible) {
			t.Fatalf("quería errAlmacenNoDisponible, dio %v", err)
		}
	}
	if f.creados.Load() != 1 {
		t.Errorf("creado %d veces", f.creados.Load())
	}
}

func TestAlmacenLlenoNoClona(t *testing.T) {
	root := t.TempDir()
	f := &almacenFalso{libre: 10 << 20}
	a := nuevoAlmacenFalso(t, root, f)
	// Almacén ya creado y montado (la raíz tendría sitio, el almacén no).
	f.montado = true
	if err := os.WriteFile(a.img, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	src := escribirDorado(t, root, "d", "x")
	_, err := a.clonarInstancia(context.Background(), "d", src, "id1", 0)
	if err == nil || errors.Is(err, errAlmacenNoDisponible) || !strings.Contains(err.Error(), "full") {
		t.Fatalf("quería 'almost full' sin degradar el modo, dio %v", err)
	}
}

// El barrido quita instancias sin máquina y bases sin dorado, y deja lo vivo.
func TestAlmacenBarrer(t *testing.T) {
	root := t.TempDir()
	f := &almacenFalso{}
	a := nuevoAlmacenFalso(t, root, f)
	src := escribirDorado(t, root, "vivo", "x")
	escribirDorado(t, root, "muerto", "y")
	for _, c := range []struct{ snap, id string }{{"vivo", "viva"}, {"vivo", "huerfana"}, {"muerto", "otra"}} {
		if _, err := a.clonarInstancia(context.Background(), c.snap, filepath.Join(root, "snapshots", c.snap, "overlay.ext4"), c.id, 0); err != nil {
			t.Fatal(err)
		}
	}
	_ = src
	if err := os.RemoveAll(filepath.Join(root, "snapshots", "muerto")); err != nil {
		t.Fatal(err)
	}
	a.barrer(func(id string) bool { return id == "viva" }, func(s string) string {
		return filepath.Join(root, "snapshots", s, "overlay.ext4")
	})
	existe := func(p string) bool { _, err := os.Lstat(p); return err == nil }
	if !existe(a.dirInstancia("viva")) {
		t.Error("borró la instancia viva")
	}
	if existe(a.dirInstancia("huerfana")) || existe(a.dirInstancia("otra")) {
		t.Error("dejó instancias huérfanas")
	}
	if existe(a.dirBases("muerto")) {
		t.Error("dejó la base de un dorado borrado")
	}
	if !existe(a.dirBases("vivo")) {
		t.Error("borró la base de un dorado vivo")
	}
}

// Sin montar, borrar no toca nada: el directorio de montaje vacío es de la
// raíz, y lo que haya ahí no es del almacén.
func TestAlmacenSinMontarNoBorra(t *testing.T) {
	root := t.TempDir()
	a := nuevoAlmacenFalso(t, root, &almacenFalso{})
	d := a.dirInstancia("id1")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	a.borrarInstancia("id1")
	a.barrer(func(string) bool { return false }, func(string) string { return "" })
	if _, err := os.Stat(d); err != nil {
		t.Errorf("borró sin estar montado: %v", err)
	}
	a.borrarInstancia("../../x") // nunca fuera del almacén
}

// clonarOverlayInstancia en modo store deja un enlace simbólico absoluto al
// almacén; si el almacén no está, copia y degrada el modo; en modo copy, copia.
func TestClonarOverlayInstanciaModos(t *testing.T) {
	m := newTestManager(t)
	f := &almacenFalso{}
	m.alm = nuevoAlmacenFalso(t, m.root, f)
	m.cow.modo = cowModoStore
	src := escribirDorado(t, m.root, "d", "contenido")
	dir := filepath.Join(m.root, "machines", "id1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "overlay.ext4")
	modo, err := m.clonarOverlayInstancia(context.Background(), "d", src, "id1", dst)
	if err != nil || modo != cowModoStore {
		t.Fatalf("modo=%q err=%v", modo, err)
	}
	if dest, err := os.Readlink(dst); err != nil || dest != m.alm.dirInstancia("id1")+"/overlay.ext4" || !filepath.IsAbs(dest) {
		t.Errorf("enlace %q %v", dest, err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "contenido" {
		t.Errorf("a través del enlace: %q", b)
	}

	// Borrar la máquina quita su overlay del almacén.
	m.borrarOverlayAlmacen("id1")
	if _, err := os.Lstat(m.alm.dirInstancia("id1")); !os.IsNotExist(err) {
		t.Errorf("el overlay sigue en el almacén: %v", err)
	}

	// Un almacén que no clona: la instancia copia, y el modo cae a copy.
	m2 := newTestManager(t)
	m2.alm = nuevoAlmacenFalso(t, m2.root, &almacenFalso{falloClonar: errors.New("EOPNOTSUPP")})
	m2.cow.modo = cowModoStore
	src2 := escribirDorado(t, m2.root, "d", "otro")
	dst2 := filepath.Join(m2.root, "overlay.ext4")
	modo, err = m2.clonarOverlayInstancia(context.Background(), "d", src2, "id2", dst2)
	if err != nil || modo != cowModoCopy {
		t.Fatalf("modo=%q err=%v", modo, err)
	}
	if fi, err := os.Lstat(dst2); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("quería un fichero normal: %v %v", fi, err)
	}
	if m2.cow.actual() != cowModoCopy {
		t.Errorf("el modo no cayó a copy: %q", m2.cow.actual())
	}
	info := m2.CoWInfo()
	if info.Mode != cowModoCopy || info.Clones[cowModoCopy] != 1 || !strings.Contains(info.Reason, "store unavailable") {
		t.Errorf("info: %+v", info)
	}
}

func TestCoWInfoSinConfigurar(t *testing.T) {
	m := newTestManager(t)
	info := m.CoWInfo()
	want := api.CoWInfo{Setting: CoWOff, Mode: cowModoCopy, Reason: "not configured"}
	if info.Setting != want.Setting || info.Mode != want.Mode || info.Store != nil {
		t.Errorf("%+v", info)
	}
}

// El daemon lee el overlay del almacén por la ruta que sale del id, no por el
// enlace de machines/<id>, que está en un directorio del VMM.
func TestOverlayParaLeer(t *testing.T) {
	m := newTestManager(t)
	m.alm = nuevoAlmacenFalso(t, m.root, &almacenFalso{})
	if got, want := m.overlayParaLeer("id1"), filepath.Join(m.dir("id1"), "overlay.ext4"); got != want {
		t.Errorf("sin almacén: %q, quería %q", got, want)
	}
	d := m.alm.dirInstancia("id1")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "overlay.ext4"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := m.overlayParaLeer("id1"); got != filepath.Join(d, "overlay.ext4") {
		t.Errorf("con almacén: %q", got)
	}
}

func TestMontadoEncimaSinMontaje(t *testing.T) {
	d := t.TempDir()
	sub := filepath.Join(d, "x")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if encima, err := montadoEncima(sub); err != nil || encima {
		t.Errorf("un directorio normal: %v %v", encima, err)
	}
	if encima, err := montadoEncima(filepath.Join(d, "no-existe")); err != nil || encima {
		t.Errorf("uno que no existe: %v %v", encima, err)
	}
}
