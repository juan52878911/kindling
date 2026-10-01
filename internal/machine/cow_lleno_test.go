package machine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Las cifras de `btrfs filesystem usage -b` del almacén de 1 GiB que se llenó
// en el laboratorio: 222 MiB libres en los chunks de datos, 1 MiB sin
// asignar y los metadatos por debajo de la reserva global. statfs ya daba 0 y
// las escrituras del invitado fallaban con EIO: lo asignable es 0.
func TestAsignableBtrfsMetadatosAgotados(t *testing.T) {
	grupos := []grupoBtrfs{
		{flags: btrfsGrupoDatos, total: 1060110336, usado: 838090752},
		{flags: btrfsGrupoMetadatos, total: 8388608, usado: 1982464},
		{flags: btrfsGrupoSistema, total: 4194304, usado: 16384},
		{flags: btrfsReservaGlobal, total: 5767168},
	}
	if got := asignableBtrfs(1073741824, 222<<20, grupos); got != 0 {
		t.Errorf("asignable = %d MiB; con los metadatos agotados tenía que ser 0", got>>20)
	}
}

// El almacén de 1,79 GiB del laboratorio antes de llenarse: metadatos de
// sobra, así que lo asignable es lo libre en datos más lo sin asignar, sin
// pasar de statfs.
func TestAsignableBtrfsDatos(t *testing.T) {
	grupos := []grupoBtrfs{
		{flags: btrfsGrupoDatos, total: 1259339776, usado: 770658304},
		{flags: btrfsGrupoMetadatos, total: 209715200, usado: 2129920},
		{flags: btrfsGrupoSistema, total: 4194304, usado: 16384},
		{flags: btrfsReservaGlobal, total: 5767168},
	}
	const tam, statfs = 1876951040, 891334656
	want := int64(1259339776-770658304) + (tam - 1259339776 - 209715200 - 4194304)
	if got := asignableBtrfs(tam, 1<<40, grupos); got != want {
		t.Errorf("asignable = %d, quería %d", got, want)
	}
	if got := asignableBtrfs(tam, statfs, grupos); got != statfs {
		t.Errorf("asignable = %d: nunca más que statfs (%d)", got, statfs)
	}
	// Mixto (datos y metadatos en los mismos chunks): la reserva sale de ahí.
	mixto := []grupoBtrfs{{flags: btrfsGrupoDatos | btrfsGrupoMetadatos, total: 1 << 30, usado: 1<<30 - 100<<20}, {flags: btrfsReservaGlobal, total: 10 << 20}}
	if got := asignableBtrfs(1<<30, 1<<40, mixto); got != 90<<20 {
		t.Errorf("mixto: asignable = %d MiB, quería 90", got>>20)
	}
}

// El error de almacén lleno dice cuánto queda y qué hacer.
func TestErrAlmacenLlenoDiceQueHacer(t *testing.T) {
	root := t.TempDir()
	f := &almacenFalso{libre: 10 << 20, montado: true}
	a := nuevoAlmacenFalso(t, root, f)
	if err := os.WriteFile(a.img, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	src := escribirDorado(t, root, "d", "x")
	_, err := a.clonarInstancia(context.Background(), "d", src, "id1", 0)
	var lleno *errAlmacenLleno
	if !errors.As(err, &lleno) {
		t.Fatalf("quería *errAlmacenLleno, dio %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "store full (10 MiB free)") || !strings.Contains(msg, "kling cow grow") {
		t.Errorf("mensaje %q", msg)
	}
}

// Una copia completa en la raíz solo si cabe entera con el margen.
func TestCabeCopiaEnRaiz(t *testing.T) {
	root := t.TempDir()
	f := &almacenFalso{}
	a := nuevoAlmacenFalso(t, root, f)
	src := filepath.Join(root, "overlay.ext4")
	if err := os.WriteFile(src, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(src, 512<<20); err != nil {
		t.Fatal(err)
	}
	f.libre = 512<<20 + margenRaizAlmacen
	if err := a.cabeCopiaEnRaiz(src); err != nil {
		t.Errorf("cabía justo: %v", err)
	}
	f.libre = 512<<20 + margenRaizAlmacen - 1
	if err := a.cabeCopiaEnRaiz(src); err == nil || !strings.Contains(err.Error(), "doesn't fit on the data root") {
		t.Errorf("no cabía: %v", err)
	}
}

// Despertar una máquina del almacén sin sitio se rechaza con el mensaje del
// almacén; una fuera del almacén no se entera.
func TestComprobarAlmacenPara(t *testing.T) {
	m := newTestManager(t)
	f := &almacenFalso{libre: 64 << 20, montado: true}
	m.alm = nuevoAlmacenFalso(t, m.root, f)
	m.alm.montado = true
	dentro, fuera := "aaaa1111", "bbbb2222"
	for _, id := range []string{dentro, fuera} {
		if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(m.alm.dirInstancia(dentro), "overlay.ext4"), filepath.Join(m.dir(dentro), "overlay.ext4")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.dir(fuera), "overlay.ext4"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var lleno *errAlmacenLleno
	if err := m.comprobarAlmacenPara(dentro); !errors.As(err, &lleno) {
		t.Errorf("dentro del almacén lleno: %v", err)
	}
	if err := m.comprobarAlmacenPara(fuera); err != nil {
		t.Errorf("fuera del almacén: %v", err)
	}
	f.libre = libreMinimaAlmacen
	if err := m.comprobarAlmacenPara(dentro); err != nil {
		t.Errorf("con sitio: %v", err)
	}
	// Y Thaw lo dice antes de lanzar nada.
	m.byID[dentro] = &api.Machine{ID: dentro, Name: "rama", State: api.StateWarm}
	f.libre = 1 << 20
	if _, err := m.Thaw(context.Background(), dentro); err == nil || !strings.Contains(err.Error(), "store full") {
		t.Errorf("Thaw con el almacén lleno: %v", err)
	}
}

func TestEscanearConsola(t *testing.T) {
	consola := strings.Join([]string{
		"[   1.0] Run /sbin/overlay-init as init process",
		"[  78.2] I/O error, dev vdb, sector 0 op 0x1:(WRITE) flags 0x3800 phys_seg 1 prio class 2",
		"[  78.3] Buffer I/O error on dev vdb, logical block 0, lost sync page write",
		"[  78.4] EXT4-fs (vdb): I/O error while writing superblock",
		"[  78.5] Aborting journal on device vdb-8.",
		"postgres: I/O error en mi tabla (no es del núcleo)",
		"[  79.0] EXT4-fs error (device vdb): ext4_journal_check_start:83: comm postgres: \x1b[31mDetected aborted journal",
		"",
	}, "\n")
	n, ultima := escanearConsola(strings.NewReader(consola))
	if n != 5 {
		t.Errorf("n = %d, quería 5", n)
	}
	if strings.ContainsRune(ultima, 0x1b) || !strings.HasPrefix(ultima, "[  79.0] EXT4-fs error") {
		t.Errorf("última = %q", ultima)
	}
}

// revisarErroresDisco anota los errores en la máquina, solo lo nuevo de cada
// vuelta, y vuelve a empezar con una consola nueva (tras un thaw).
func TestRevisarErroresDisco(t *testing.T) {
	m := newTestManager(t)
	id := "cccc3333"
	m.byID[id] = &api.Machine{ID: id, Name: "copia", State: api.StateRunning}
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := abrirConsola(m.dir(id))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, _ = f.WriteString("arranque\n[ 9.0] I/O error, dev vdb, sector 8 op 0x1:(WRITE)\n")
	m.revisarErroresDisco()
	m.revisarErroresDisco() // nada nuevo: no cuenta dos veces
	mc, _ := m.Get(id)
	if mc.DiskErrors != 1 || mc.DiskErrorAt == nil || !strings.Contains(mc.DiskError, "dev vdb") {
		t.Fatalf("tras la primera: %+v", mc)
	}
	_, _ = f.WriteString("[ 9.1] Aborting journal on device vdb-8.\n[ 9.2] a medias I/O error, dev vd")
	m.revisarErroresDisco()
	if mc, _ := m.Get(id); mc.DiskErrors != 2 {
		t.Errorf("tras la segunda: %d errores, quería 2 (la línea a medias aún no)", mc.DiskErrors)
	}
}

// La marca de pausa sube con lo que se está gastando, y la espera entre
// vueltas baja cuanto menos queda.
func TestVigiaMarcaYEspera(t *testing.T) {
	v := &vigia{}
	t0 := time.Now()
	v.medir(t0, 900<<20)
	if v.marca() != marcaPausaAlmacen {
		t.Errorf("sin tasa, marca = %d MiB", v.marca()>>20)
	}
	if e := v.espera(); e < vigiaAlmacenMin || e > vigiaAlmacenMax {
		t.Errorf("espera fuera de rango: %v", e)
	}
	// 400 MiB en 100 ms: 4000 MiB/s; marca = 128 + 2000 MiB.
	v.medir(t0.Add(100*time.Millisecond), 500<<20)
	if m := v.marca() >> 20; m < 2000 || m > 2200 {
		t.Errorf("marca = %d MiB, quería ~2128", m)
	}
	if v.espera() != vigiaAlmacenMin {
		t.Errorf("por debajo de la marca, espera = %v; quería la mínima", v.espera())
	}
	// En ventanas de menos de 100 ms no se recalcula: el ruido no es tasa.
	antes := v.marca()
	v.medir(t0.Add(110*time.Millisecond), 100<<20)
	if v.marca() != antes {
		t.Errorf("una medida a 10 ms cambió la marca: %d → %d MiB", antes>>20, v.marca()>>20)
	}
	v.medir(t0.Add(120*time.Millisecond), 500<<20)
	// Sin gastar nada durante 10 s, la tasa se olvida.
	v.medir(t0.Add(10*time.Second), 500<<20)
	if v.marca()>>20 > 150 {
		t.Errorf("tras 10 s quietos, marca = %d MiB", v.marca()>>20)
	}
	if v.espera() <= vigiaAlmacenMin {
		t.Errorf("con sitio y sin gasto, espera = %v", v.espera())
	}
}
