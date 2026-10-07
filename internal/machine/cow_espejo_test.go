package machine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Con el espejo hecho tras el commit (espejarMemoria), el thaw no copia nada:
// clona el espejo y escribe el diff. Sin él, el thaw paga la copia entera, y
// lo dice en sus tiempos.
func TestEspejoTrasCommitAhorraLaCopiaDelThaw(t *testing.T) {
	root := t.TempDir()
	f := &almacenFalso{}
	a := nuevoAlmacenFalso(t, root, f)
	diff := filepath.Join(root, "diff")
	escribirPaginas(t, diff, nil, paginaDe('D'))

	src := escribirMemoriaDorada(t, root, "con", paginaDe('A'), paginaDe('B'))
	if err := a.espejarMemoria(context.Background(), "con", src, 0); err != nil {
		t.Fatal(err)
	}
	antes := f.copias.Load()
	if antes != 1 {
		t.Fatalf("copias=%d tras espejar: el espejo es una copia", antes)
	}
	var tm tiemposMemoria
	if _, err := a.memoriaInstancia(context.Background(), "con", src, diff, "copia1", 0, &tm); err != nil {
		t.Fatal(err)
	}
	if n := f.copias.Load() - antes; n != 0 {
		t.Errorf("el thaw copió %d veces con el espejo ya hecho", n)
	}

	// Sin espejo previo: el thaw lo copia (el camino perezoso sigue).
	src2 := escribirMemoriaDorada(t, root, "sin", paginaDe('A'))
	antes = f.copias.Load()
	if _, err := a.memoriaInstancia(context.Background(), "sin", src2, diff, "copia2", 0, &tm); err != nil {
		t.Fatal(err)
	}
	if n := f.copias.Load() - antes; n != 1 {
		t.Errorf("sin espejo previo el thaw tenía que copiarlo una vez, copió %d", n)
	}
}

// Mientras el espejo se copia, el almacén no está bloqueado: un run -from
// clona, barrer no se lleva el temporal, y un thaw que lo necesita espera a
// esa copia en vez de hacer otra.
func TestEspejoEnCursoNoBloqueaElAlmacen(t *testing.T) {
	root := t.TempDir()
	f := &almacenFalso{}
	a := nuevoAlmacenFalso(t, root, f)
	empezo, soltar := make(chan struct{}), make(chan struct{})
	var copiasMem atomic.Int32
	copiar := a.copiar
	a.copiar = func(ctx context.Context, src, dst string) error {
		err := copiar(ctx, src, dst)
		// La primera copia del espejo se queda a medias (el temporal ya
		// existe) hasta que la prueba la suelte.
		if strings.HasSuffix(dst, sufijoBaseMemoria) && copiasMem.Add(1) == 1 {
			close(empezo)
			<-soltar
		}
		return err
	}
	src := escribirMemoriaDorada(t, root, "dorado", paginaDe('A'), paginaDe('B'))
	ov := escribirDorado(t, root, "dorado", "disco")
	espejado := make(chan error, 1)
	go func() { espejado <- a.espejarMemoria(context.Background(), "dorado", src, 0) }()
	<-empezo

	hecho := make(chan error, 1)
	go func() {
		_, err := a.clonarInstancia(context.Background(), "dorado", ov, "otra", 0)
		hecho <- err
	}()
	select {
	case err := <-hecho:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		close(soltar)
		t.Fatal("un run -from esperó a la copia del espejo")
	}
	a.barrer(func(id string) bool { return true }, func(s string) string {
		return filepath.Join(root, "snapshots", s, "overlay.ext4")
	}, func(s string) string {
		return filepath.Join(root, "snapshots", s, "mem.file")
	})
	tmps, _ := filepath.Glob(filepath.Join(root, "cow", "bases", "dorado", ".tmp-*"+sufijoBaseMemoria))
	if len(tmps) != 1 {
		t.Errorf("barrer se llevó el temporal del espejo en curso: %v", tmps)
	}

	diff := filepath.Join(root, "diff")
	escribirPaginas(t, diff, nil, paginaDe('D'))
	thaw := make(chan error, 1)
	var tm tiemposMemoria
	go func() {
		_, err := a.memoriaInstancia(context.Background(), "dorado", src, diff, "copia", 0, &tm)
		thaw <- err
	}()
	select {
	case err := <-thaw:
		t.Fatalf("el thaw no esperó al espejo en curso: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(soltar)
	if err := <-espejado; err != nil {
		t.Fatal(err)
	}
	if err := <-thaw; err != nil {
		t.Fatal(err)
	}
	if n := copiasMem.Load(); n != 1 {
		t.Errorf("el espejo se copió %d veces", n)
	}
	if tm.espejo < 40*time.Millisecond {
		t.Errorf("la espera al espejo (%s) tenía que contar como mirror", tm.espejo)
	}
}

// El commit lanza el espejo en segundo plano (espejarMemoriaDorado) y el
// primer thaw de una copia congelada en diferencial ya no copia.
func TestEspejarMemoriaDoradoEnSegundoPlano(t *testing.T) {
	if !congelarEnDiff() {
		t.Skip("sin congelado diferencial en esta plataforma")
	}
	m := newTestManager(t)
	t.Setenv("KLING_MIN_FREE_DISK_MIB", "0")
	f := &almacenFalso{}
	m.alm = nuevoAlmacenFalso(t, m.root, f)
	m.cow.modo = cowModoStore
	base := escribirMemoriaDorada(t, m.root, "dorado", paginaDe('A'), paginaDe('B'))
	if base != filepath.Join(m.snapDir("dorado"), "mem.file") {
		t.Fatalf("el dorado de la prueba no está donde lo busca el Manager: %s", base)
	}
	m.espejarMemoriaDorado("dorado")
	plazo := time.Now().Add(5 * time.Second)
	for f.copias.Load() == 0 || len(espejosHechos(t, m.root, "dorado")) == 0 {
		if time.Now().After(plazo) {
			t.Fatal("el espejo no se hizo en segundo plano")
		}
		time.Sleep(5 * time.Millisecond)
	}

	id := "0123456789abcdef"
	dir := filepath.Join(m.root, "machines", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !sabeDeHuecos(t, dir) {
		t.Skip("este sistema de ficheros no distingue huecos")
	}
	escribirPaginas(t, filepath.Join(dir, "mem.file"), nil, paginaDe('D'))
	antes := f.copias.Load()
	var tm tiemposMemoria
	full, err := m.prepararMemoriaDesdeDiff(context.Background(), &api.Machine{ID: id, MemMiB: 1}, dir, base, &tm)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.copias.Load() - antes; n != 0 {
		t.Errorf("el thaw copió %d veces con el espejo del commit hecho", n)
	}
	if fi, err := os.Lstat(full); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("la memoria no salió del almacén: %v", err)
	}
}

func espejosHechos(t *testing.T, root, snap string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(root, "cow", "bases", snap, "[!.]*"+sufijoBaseMemoria))
	return m
}

// El desglose del thaw dice lo que costaron el almacén y el espejo, sin
// contarlos dos veces en la memoria.
func TestNotaFasesConEspejo(t *testing.T) {
	c := nuevoCrono("frozen")
	c.ultima = c.ultima.Add(-300 * time.Millisecond)
	c.marcaMemoria(tiemposMemoria{almacen: 20 * time.Millisecond, espejo: 250 * time.Millisecond})
	p := c.cerrar()
	if p.StoreMS < 19 || p.MirrorMS < 249 || p.MemoryMS < 25 || p.MemoryMS > 100 {
		t.Fatalf("fases: store %.1f mirror %.1f memory %.1f", p.StoreMS, p.MirrorMS, p.MemoryMS)
	}
	n := notaFases(p)
	for _, w := range []string{"store 20", "mirror 250", "memory "} {
		if !strings.Contains(n, w) {
			t.Errorf("%q no dice %q", n, w)
		}
	}
}
