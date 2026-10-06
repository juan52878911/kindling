package machine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// escribirMemoriaDorada deja el mem.file del dorado snap con las páginas
// dadas (reemplazándolo como hace commit: otro inodo).
func escribirMemoriaDorada(t *testing.T, root, snap string, paginas ...[]byte) string {
	t.Helper()
	dir := filepath.Join(root, "snapshots", snap)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ruta := filepath.Join(dir, "mem.file")
	tmp := ruta + ".nuevo"
	escribirPaginas(t, tmp, paginas...)
	if err := os.Rename(tmp, ruta); err != nil {
		t.Fatal(err)
	}
	return ruta
}

// El espejo de la memoria del dorado se copia UNA vez; cada copia recibe un
// clon con su diff encima, legible y no escribible por el VMM, y la base y
// las demás copias no ven ese diff.
func TestAlmacenEspejaLaMemoriaDelDorado(t *testing.T) {
	root := t.TempDir()
	if !sabeDeHuecos(t, root) {
		t.Skip("este sistema de ficheros no distingue huecos")
	}
	f := &almacenFalso{}
	a := nuevoAlmacenFalso(t, root, f)
	src := escribirMemoriaDorada(t, root, "dorado", paginaDe('A'), paginaDe('B'))

	// copia2 tiene su overlay en el almacén, como una copia normal (runFrom
	// lo clona antes de que nadie piense en su memoria); copia1 no.
	ov := escribirDorado(t, root, "dorado", "disco")
	if _, err := a.clonarInstancia(context.Background(), "dorado", ov, "copia2", 0); err != nil {
		t.Fatal(err)
	}
	diffs := map[string]string{}
	for i, letra := range []byte{'1', '2'} {
		id := "copia" + string(letra)
		mdir := filepath.Join(root, "machines", id)
		if err := os.MkdirAll(mdir, 0o755); err != nil {
			t.Fatal(err)
		}
		diff := filepath.Join(mdir, "mem.file")
		escribirPaginas(t, diff, nil, paginaDe(letra))
		diffs[id] = diff
		ruta, err := a.memoriaInstancia(context.Background(), "dorado", src, diff, id, 0)
		if err != nil {
			t.Fatalf("copia %d: %v", i, err)
		}
		if ruta != filepath.Join(root, "cow", "m", id, memFull) {
			t.Errorf("ruta %q", ruta)
		}
		got := leerPaginas(t, ruta, 2)
		if !bytes.Equal(got[0], paginaDe('A')) || !bytes.Equal(got[1], paginaDe(letra)) {
			t.Errorf("copia %s: no es base + su diff", id)
		}
		if fi, _ := os.Stat(ruta); fi.Mode().Perm() != 0o640 {
			t.Errorf("modo %v: el VMM solo debe poder leerlo", fi.Mode().Perm())
		}
	}
	// Dos copias completas en total: la base del overlay y el espejo.
	if f.copias.Load() != 2 {
		t.Errorf("copias=%d: el espejo se copia una vez por dorado", f.copias.Load())
	}
	if got := leerPaginas(t, src, 2); !bytes.Equal(got[1], paginaDe('B')) {
		t.Error("el diff de una copia llegó al mem.file del dorado")
	}
	bases, _ := os.ReadDir(filepath.Join(root, "cow", "bases", "dorado"))
	if len(bases) != 2 {
		t.Errorf("bases: %v, quería el overlay y el espejo", bases)
	}

	// Sin overlay en el almacén, borrar la memoria se lleva el directorio.
	a.borrarMemoriaInstancia("copia1")
	if _, err := os.Lstat(a.dirInstancia("copia1")); !os.IsNotExist(err) {
		t.Error("el directorio de copia1 estaba solo para la memoria y sigue ahí")
	}
	// Con overlay, se queda el directorio y el overlay.
	a.borrarMemoriaInstancia("copia2")
	if _, err := os.Lstat(filepath.Join(a.dirInstancia("copia2"), "overlay.ext4")); err != nil {
		t.Error("borrar la memoria se llevó el overlay de copia2")
	}
	if _, err := os.Lstat(filepath.Join(a.dirInstancia("copia2"), memFull)); !os.IsNotExist(err) {
		t.Error("mem.full de copia2 sigue ahí")
	}

	// Dorado reemplazado: espejo nuevo, el viejo se va; el .ext4 de la base
	// del overlay no se toca.
	time.Sleep(10 * time.Millisecond)
	src = escribirMemoriaDorada(t, root, "dorado", paginaDe('X'), paginaDe('Y'))
	ruta, err := a.memoriaInstancia(context.Background(), "dorado", src, diffs["copia2"], "copia2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := leerPaginas(t, ruta, 2); !bytes.Equal(got[0], paginaDe('X')) {
		t.Error("tras reemplazar el dorado la copia no sale del espejo nuevo")
	}
	bases, _ = os.ReadDir(filepath.Join(root, "cow", "bases", "dorado"))
	var mems, ext4s int
	for _, b := range bases {
		switch filepath.Ext(b.Name()) {
		case sufijoBaseMemoria:
			mems++
		case ".ext4":
			ext4s++
		}
	}
	if mems != 1 || ext4s != 1 {
		t.Errorf("bases tras reemplazar: %d espejos y %d overlays, quería 1 y 1", mems, ext4s)
	}
}

// barrer conserva el espejo vigente del dorado y quita el de una versión
// anterior, sin tocar la base del overlay.
func TestAlmacenBarrerConservaElEspejoVigente(t *testing.T) {
	root := t.TempDir()
	f := &almacenFalso{}
	a := nuevoAlmacenFalso(t, root, f)
	ov := escribirDorado(t, root, "dorado", "disco")
	if _, err := a.clonarInstancia(context.Background(), "dorado", ov, "viva", 0); err != nil {
		t.Fatal(err)
	}
	src := escribirMemoriaDorada(t, root, "dorado", paginaDe('A'))
	a.mu.Lock()
	espejo, err := a.baseMemoria(context.Background(), "dorado", src)
	a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	viejo := filepath.Join(filepath.Dir(espejo), "0-0-0-0"+sufijoBaseMemoria)
	if err := os.WriteFile(viejo, []byte("viejo"), 0o400); err != nil {
		t.Fatal(err)
	}
	a.barrer(func(id string) bool { return id == "viva" }, func(s string) string {
		return filepath.Join(root, "snapshots", s, "overlay.ext4")
	}, func(s string) string {
		return filepath.Join(root, "snapshots", s, "mem.file")
	})
	existe := func(p string) bool { _, err := os.Lstat(p); return err == nil }
	if !existe(espejo) {
		t.Error("barrer quitó el espejo vigente")
	}
	if existe(viejo) {
		t.Error("barrer dejó el espejo de una versión anterior")
	}
	bases, _ := os.ReadDir(filepath.Dir(espejo))
	if len(bases) != 2 {
		t.Errorf("bases: %v, quería el overlay y el espejo", bases)
	}
}

// Sin sitio en el almacén para el espejo o para el diff, se dice con
// errAlmacenLleno y quien llama sigue por la raíz.
func TestAlmacenSinSitioNoEspeja(t *testing.T) {
	root := t.TempDir()
	f := &almacenFalso{}
	a := nuevoAlmacenFalso(t, root, f)
	src := escribirMemoriaDorada(t, root, "dorado", paginaDe('A'))
	diff := filepath.Join(root, "diff")
	escribirPaginas(t, diff, paginaDe('D'))
	// Con el almacén ya montado (si no, el fallo sería "no disponible").
	if _, err := a.memoriaInstancia(context.Background(), "dorado", src, diff, "primera", 0); err != nil {
		t.Fatal(err)
	}
	f.libre = 64 << 20
	_, err := a.memoriaInstancia(context.Background(), "dorado", src, diff, "copia", 0)
	var lleno *errAlmacenLleno
	if !errors.As(err, &lleno) {
		t.Fatalf("con 64 MiB libres tenía que decir almacén lleno: %v", err)
	}
	if _, err := os.Lstat(a.dirInstancia("copia")); !os.IsNotExist(err) {
		t.Error("dejó el directorio de la copia")
	}
}
