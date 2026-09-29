package machine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// La cuota de una instancia es el tamaño lógico del overlay más holgura.
func TestCuotaInstancia(t *testing.T) {
	if got, want := cuotaInstancia(512<<20), int64(512<<20+16<<20+16<<20); got != want {
		t.Errorf("cuotaInstancia(512 MiB) = %d, quería %d", got, want)
	}
	if cuotaInstancia(0) <= 0 || cuotaInstancia(1<<30) <= 1<<30 {
		t.Error("la cuota tiene que dejar holgura sobre el tamaño lógico")
	}
}

// Las opciones de montaje del superbloque llegan a montaje.opts.
func TestParsearMountinfoOpciones(t *testing.T) {
	ms, err := parsearMountinfo(strings.NewReader(
		"36 35 7:0 / /var/lib/kindling/cow rw,nosuid,nodev,noexec,relatime shared:1 - xfs /dev/loop0 rw,attr2,inode64,prjquota\n"))
	if err != nil || len(ms) != 1 {
		t.Fatalf("%v %v", ms, err)
	}
	for _, o := range []string{"noexec", "prjquota"} {
		if !strings.Contains(ms[0].opts, o) {
			t.Errorf("opts %q sin %s", ms[0].opts, o)
		}
	}
}

type cuotaFalsa struct {
	mu       sync.Mutex
	llamadas []int64
	dirs     []string
	falla    error
}

// con engancha los hooks de cuota al almacén de pruebas.
func (c *cuotaFalsa) con(a *almacenCoW) {
	a.detectarCuota = func(context.Context) string { return "prjquota" }
	a.limitar = func(d, overlay string, bytes int64) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.falla != nil {
			return c.falla
		}
		c.llamadas = append(c.llamadas, bytes)
		c.dirs = append(c.dirs, d)
		return nil
	}
}

// Con cuota, cada instancia se limita al tamaño lógico de su overlay (más
// holgura), la clonación y el borrado siguen funcionando, y si la cuota no se
// puede aplicar la instancia no entra al almacén ni deja residuos.
func TestAlmacenAplicaCuota(t *testing.T) {
	root := t.TempDir()
	f := &almacenFalso{}
	a := nuevoAlmacenFalso(t, root, f)
	c := &cuotaFalsa{}
	c.con(a)
	src := escribirDorado(t, root, "dorado", "disco v1")

	ruta, err := a.clonarInstancia(context.Background(), "dorado", src, "id1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if a.cuota != "prjquota" {
		t.Errorf("cuota detectada %q", a.cuota)
	}
	if len(c.llamadas) != 1 || c.llamadas[0] != cuotaInstancia(int64(len("disco v1"))) {
		t.Errorf("llamadas de cuota: %v", c.llamadas)
	}
	if c.dirs[0] != filepath.Dir(ruta) {
		t.Errorf("cuota sobre %q, quería %q", c.dirs[0], filepath.Dir(ruta))
	}
	if s := a.info(); s == nil || s.Quota != "prjquota" || s.NoQuota {
		t.Errorf("info: %+v", s)
	}

	// Falla la cuota: error, y ni directorio ni overlay.
	c.falla = errors.New("EDQUOT de mentira")
	if _, err := a.clonarInstancia(context.Background(), "dorado", src, "id2", 0); err == nil || !strings.Contains(err.Error(), "quota") {
		t.Errorf("con la cuota rota tendría que fallar: %v", err)
	}
	if _, err := os.Lstat(a.dirInstancia("id2")); !os.IsNotExist(err) {
		t.Errorf("quedó el directorio de la instancia sin cuota: %v", err)
	}

	// El borrado y el barrido siguen funcionando con cuota.
	a.borrarInstancia("id1")
	if _, err := os.Lstat(a.dirInstancia("id1")); !os.IsNotExist(err) {
		t.Errorf("borrarInstancia con cuota: %v", err)
	}
	c.falla = nil
	if _, err := a.clonarInstancia(context.Background(), "dorado", src, "id3", 0); err != nil {
		t.Fatal(err)
	}
	a.barrer(func(string) bool { return false }, func(string) string { return src })
	if _, err := os.Lstat(a.dirInstancia("id3")); !os.IsNotExist(err) {
		t.Errorf("barrer con cuota: %v", err)
	}
}

// Sin cuota (sin herramientas o montaje sin ella) el almacén funciona como
// antes, y info lo dice para que doctor avise.
func TestAlmacenSinCuota(t *testing.T) {
	root := t.TempDir()
	a := nuevoAlmacenFalso(t, root, &almacenFalso{})
	a.detectarCuota = func(context.Context) string { return "" }
	a.limitar = func(string, string, int64) error {
		t.Error("limitar sin cuota detectada")
		return nil
	}
	src := escribirDorado(t, root, "dorado", "x")
	if _, err := a.clonarInstancia(context.Background(), "dorado", src, "id1", 0); err != nil {
		t.Fatal(err)
	}
	if s := a.info(); s == nil || s.Quota != "" || !s.NoQuota {
		t.Errorf("info: %+v", s)
	}
}
