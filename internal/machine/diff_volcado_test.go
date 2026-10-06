package machine

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

const pagina = 4096

// escribirPaginas deja en path las páginas dadas: nil es un hueco (no se
// escribe), cualquier otra cosa se escribe tal cual, ceros incluidos.
func escribirPaginas(t *testing.T, path string, paginas ...[]byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(int64(len(paginas) * pagina)); err != nil {
		t.Fatal(err)
	}
	for i, p := range paginas {
		if p == nil {
			continue
		}
		if _, err := f.WriteAt(p, int64(i*pagina)); err != nil {
			t.Fatal(err)
		}
	}
}

func paginaDe(b byte) []byte { return bytes.Repeat([]byte{b}, pagina) }

func leerPaginas(t *testing.T, path string, n int) [][]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != n*pagina {
		t.Fatalf("%s mide %d bytes, quería %d", path, len(b), n*pagina)
	}
	out := make([][]byte, n)
	for i := range out {
		out[i] = b[i*pagina : (i+1)*pagina]
	}
	return out
}

// sabeDeHuecos dice si el sistema de ficheros del directorio distingue huecos
// (SEEK_HOLE): sin eso un hueco se lee como datos a cero y los tests de
// diferenciales no pueden distinguir los dos casos.
func sabeDeHuecos(t *testing.T, dir string) bool {
	t.Helper()
	p := filepath.Join(dir, "sonda")
	escribirPaginas(t, p, nil, paginaDe('x'))
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ini, _ := siguientesDatos(f, 0, 2*pagina)
	return ini == pagina
}

// En un diferencial, un hueco es "como la base" y una página de ceros es "el
// invitado la puso a cero": al aplicarlo, la primera no se toca y la segunda
// se escribe.
func TestAplicarDiffRespetaHuecosYEscribeCeros(t *testing.T) {
	dir := t.TempDir()
	if !sabeDeHuecos(t, dir) {
		t.Skip("este sistema de ficheros no distingue huecos")
	}
	base := filepath.Join(dir, "base")
	escribirPaginas(t, base, paginaDe('A'), paginaDe('B'), paginaDe('C'))
	diff := filepath.Join(dir, "diff")
	escribirPaginas(t, diff, nil, make([]byte, pagina), paginaDe('Z'))
	if err := aplicarDiff(context.Background(), diff, base); err != nil {
		t.Fatal(err)
	}
	got := leerPaginas(t, base, 3)
	if !bytes.Equal(got[0], paginaDe('A')) {
		t.Error("el hueco del diff pisó la página de la base")
	}
	if !bytes.Equal(got[1], make([]byte, pagina)) {
		t.Error("la página a ceros del diff no se escribió: el invitado la había puesto a cero")
	}
	if !bytes.Equal(got[2], paginaDe('Z')) {
		t.Error("la página con datos del diff no se escribió")
	}
}

// El primer freeze deja el diff como mem.file; los siguientes se funden
// encima, con lo nuevo ganando, y mem.full desaparece.
func TestFusionarDiffAcumula(t *testing.T) {
	dir := t.TempDir()
	if !sabeDeHuecos(t, dir) {
		t.Skip("este sistema de ficheros no distingue huecos")
	}
	escribirPaginas(t, filepath.Join(dir, memDiff), nil, paginaDe('1'), nil)
	if err := fusionarDiff(context.Background(), dir, filepath.Join(dir, memDiff), filepath.Join(dir, "mem.file")); err != nil {
		t.Fatal(err)
	}
	if existe(filepath.Join(dir, memDiff)) {
		t.Fatal("el primer diff tenía que pasar a ser mem.file")
	}
	escribirPaginas(t, filepath.Join(dir, memFull), paginaDe('f'))
	escribirPaginas(t, filepath.Join(dir, memDiff), nil, nil, paginaDe('2'))
	if err := fusionarDiff(context.Background(), dir, filepath.Join(dir, memDiff), filepath.Join(dir, "mem.file")); err != nil {
		t.Fatal(err)
	}
	if existe(filepath.Join(dir, memDiff)) || existe(filepath.Join(dir, memFull)) {
		t.Fatal("tras fundir no deben quedar ni mem.diff ni mem.full")
	}
	got := leerPaginas(t, filepath.Join(dir, "mem.file"), 3)
	if !bytes.Equal(got[1], paginaDe('1')) || !bytes.Equal(got[2], paginaDe('2')) {
		t.Errorf("el diff acumulado no tiene las dos escrituras")
	}
	f, _ := os.Open(filepath.Join(dir, "mem.file"))
	defer f.Close()
	if ini, _ := siguientesDatos(f, 0, 3*pagina); ini != pagina {
		t.Errorf("la página 0 tenía que seguir siendo un hueco (como la base); los datos empiezan en %d", ini)
	}
}

// Para descongelar se construye base + diff en un fichero propio; sin la base
// no hay nada que cargar y se dice.
func TestPrepararMemoriaDesdeDiff(t *testing.T) {
	m := newTestManager(t)
	t.Setenv("KLING_MIN_FREE_DISK_MIB", "0")
	dir := filepath.Join(m.root, "machines", "0123456789abcdef")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !sabeDeHuecos(t, dir) {
		t.Skip("este sistema de ficheros no distingue huecos")
	}
	base := filepath.Join(m.root, "base.mem")
	escribirPaginas(t, base, paginaDe('A'), paginaDe('B'))
	escribirPaginas(t, filepath.Join(dir, "mem.file"), nil, paginaDe('D'))
	mc := &api.Machine{ID: "0123456789abcdef", MemMiB: 1}
	full, err := m.prepararMemoriaDesdeDiff(context.Background(), mc, dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if full != filepath.Join(dir, memFull) {
		t.Fatalf("la memoria para cargar es %s", full)
	}
	got := leerPaginas(t, full, 2)
	if !bytes.Equal(got[0], paginaDe('A')) || !bytes.Equal(got[1], paginaDe('D')) {
		t.Error("mem.full no es base + diff")
	}
	if b := leerPaginas(t, base, 2); !bytes.Equal(b[1], paginaDe('B')) {
		t.Error("se escribió sobre la base del dorado")
	}
	if _, err := m.prepararMemoriaDesdeDiff(context.Background(), mc, dir, filepath.Join(m.root, "no-esta")); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("sin la base tenía que decirlo: %v", err)
	}
}

// Una copia con base congela en diferencial: pide un Diff al VMM, el diff
// queda como su mem.file sin perforar, el sello apunta a la base y la máquina
// warm la conserva para el siguiente ciclo. Apagado por entorno, vuelca
// entero como siempre.
func TestFreezeDiferencial(t *testing.T) {
	if !restaurarComparteMemoria {
		t.Skip("en esta plataforma las copias no comparten memoria con el dorado")
	}
	for _, apagado := range []bool{false, true} {
		m := newTestManager(t)
		m.bus = events.New()
		t.Setenv("KLING_MIN_FREE_DISK_MIB", "0")
		if apagado {
			t.Setenv("KLING_DIFF_FREEZE", "0")
		} else {
			t.Setenv("KLING_DIFF_FREEZE", "")
		}
		id := newID()
		dir := m.dir(id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		base := filepath.Join(m.root, "dorado.mem")
		escribirPaginas(t, base, paginaDe('A'), paginaDe('B'))
		falso := nuevoFcFalso(t)
		mc := m.addForTest(id)
		m.mu.Lock()
		mc.DiffBase = base
		mc.MemMiB = 1
		m.socket[id] = falso.Sock
		m.mu.Unlock()
		// El VMM falso no escribe nada: lo que volcaría se deja aquí, en el
		// momento en que llega la petición.
		falso.enGancho(func(metodo, ruta string) {
			if metodo != http.MethodPut || ruta != "/snapshot/create" {
				return
			}
			_ = os.WriteFile(filepath.Join(dir, "snap.file"), []byte("snap"), 0o644)
			if apagado {
				escribirPaginas(t, filepath.Join(dir, "mem.file"), paginaDe('A'), paginaDe('D'))
			} else {
				escribirPaginas(t, filepath.Join(dir, memDiff), nil, paginaDe('D'))
			}
		})

		if _, err := m.Freeze(context.Background(), id); err != nil {
			t.Fatalf("apagado=%v: Freeze: %v", apagado, err)
		}
		var tipo string
		for _, l := range falso.todas() {
			if l.Ruta == "/snapshot/create" {
				tipo = string(l.Cuerpo)
			}
		}
		quiere := `"Diff"`
		if apagado {
			quiere = `"Full"`
		}
		if !strings.Contains(tipo, quiere) {
			t.Fatalf("apagado=%v: el volcado pedido fue %s, quería %s", apagado, tipo, quiere)
		}
		viva := vivaDe(t, m, id)
		if viva.State != api.StateWarm {
			t.Fatalf("estado %s", viva.State)
		}
		sello := leerSello(dir)
		if apagado {
			if sello.DiffBase != "" {
				t.Fatal("apagado: el sello no debe llevar base")
			}
			continue
		}
		if sello.DiffBase != base || viva.DiffBase != base {
			t.Fatalf("sello %q, máquina %q: la base tenía que ser %s", sello.DiffBase, viva.DiffBase, base)
		}
		if existe(filepath.Join(dir, memDiff)) {
			t.Fatal("mem.diff tenía que fundirse en mem.file")
		}
		if sabeDeHuecos(t, dir) {
			f, _ := os.Open(filepath.Join(dir, "mem.file"))
			if ini, _ := siguientesDatos(f, 0, 2*pagina); ini != pagina {
				t.Errorf("el diff se perforó o se rellenó: los datos empiezan en %d", ini)
			}
			f.Close()
		}
	}
}

// Un volcado completo (commit, un diff fallido) deja a la copia sin base: su
// siguiente freeze vuelca entero.
func TestOlvidarDiffBase(t *testing.T) {
	m := newTestManager(t)
	id := newID()
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.DiffBase = "/algo"
	m.mu.Unlock()
	m.olvidarDiffBase(id)
	if v := vivaDe(t, m, id); v.DiffBase != "" {
		t.Fatalf("sigue con base %q", v.DiffBase)
	}
	m.olvidarDiffBase("no-existe")
}

// Un freeze completo de una copia que despertó de un diferencial (y perdió su
// base por un commit, o con el diff apagado) retira el mem.full que mapeaba
// el VMM: muerto este, son GiB por copia dormida que nadie lee.
func TestFreezeCompletoRetiraMemFull(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	t.Setenv("KLING_MIN_FREE_DISK_MIB", "0")
	id := newID()
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	escribirPaginas(t, filepath.Join(dir, memFull), paginaDe('F'))
	falso := nuevoFcFalso(t)
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.MemMiB = 1
	m.socket[id] = falso.Sock
	m.mu.Unlock()
	falso.enGancho(func(metodo, ruta string) {
		if metodo == http.MethodPut && ruta == "/snapshot/create" {
			_ = os.WriteFile(filepath.Join(dir, "snap.file"), []byte("snap"), 0o644)
			escribirPaginas(t, filepath.Join(dir, "mem.file"), paginaDe('A'))
		}
	})
	if _, err := m.Freeze(context.Background(), id); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if existe(filepath.Join(dir, memFull)) {
		t.Fatal("mem.full sigue ahí tras un freeze completo")
	}
	if leerSello(dir).DiffBase != "" {
		t.Fatal("un freeze completo no lleva base")
	}
}

// Con el diff en el almacén, machines/<id>/mem.file es un enlace a él, y el
// acumulado se funde allí.
func TestFusionarDiffEnElAlmacen(t *testing.T) {
	dir := t.TempDir()
	if !sabeDeHuecos(t, dir) {
		t.Skip("este sistema de ficheros no distingue huecos")
	}
	alm := filepath.Join(dir, "almacen")
	if err := os.MkdirAll(alm, 0o755); err != nil {
		t.Fatal(err)
	}
	diff, acum := filepath.Join(alm, memDiff), filepath.Join(alm, "mem.file")
	escribirPaginas(t, diff, nil, paginaDe('1'))
	if err := fusionarDiff(context.Background(), dir, diff, acum); err != nil {
		t.Fatal(err)
	}
	if dest, err := os.Readlink(filepath.Join(dir, "mem.file")); err != nil || dest != acum {
		t.Fatalf("machines/<id>/mem.file = %q (%v), quería un enlace a %s", dest, err, acum)
	}
	escribirPaginas(t, diff, paginaDe('0'), nil)
	if err := fusionarDiff(context.Background(), dir, diff, acum); err != nil {
		t.Fatal(err)
	}
	got := leerPaginas(t, filepath.Join(dir, "mem.file"), 2)
	if !bytes.Equal(got[0], paginaDe('0')) || !bytes.Equal(got[1], paginaDe('1')) {
		t.Error("el acumulado del almacén no tiene las dos escrituras")
	}
	if existe(diff) {
		t.Error("mem.diff sigue en el almacén tras fundirlo")
	}
}

// Antes de un volcado completo, o cuando un diff deja de valer, el acumulado
// del almacén y su enlace se retiran: si no, el volcado entero iría al
// almacén a través del enlace y el acumulado quedaría huérfano bajo la cuota.
func TestBorrarAcumuladoDiff(t *testing.T) {
	m := newTestManager(t)
	f := &almacenFalso{}
	a := nuevoAlmacenFalso(t, m.root, f)
	m.alm = a
	if _, err := a.memoriaInstancia(context.Background(), "x", "", "", "", 0); err == nil {
		t.Fatal("no debía poder sin fuentes")
	}
	a.mu.Lock()
	if err := a.preparar(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	a.mu.Unlock()
	id := newID()
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(a.dirInstancia(id), 0o755); err != nil {
		t.Fatal(err)
	}
	acum := a.acumuladoDiff(id)
	escribirPaginas(t, acum, paginaDe('d'))
	if err := os.Symlink(acum, filepath.Join(dir, "mem.file")); err != nil {
		t.Fatal(err)
	}
	m.borrarAcumuladoDiff(id, dir)
	if existe(acum) || existe(filepath.Join(dir, "mem.file")) {
		t.Fatal("el acumulado o su enlace siguen ahí")
	}
	// Un mem.file regular (un diff fuera del almacén) no se toca aquí.
	escribirPaginas(t, filepath.Join(dir, "mem.file"), paginaDe('r'))
	m.borrarAcumuladoDiff(id, dir)
	if !existe(filepath.Join(dir, "mem.file")) {
		t.Fatal("borró un diff regular que no es del almacén")
	}
}

// Un freeze completo de una copia cuyo acumulado vivía en el almacén quita
// el enlace ANTES de volcar: el mem.file del volcado es un fichero propio y
// el acumulado del almacén desaparece.
func TestFreezeCompletoNoEscribeEnElAlmacen(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	t.Setenv("KLING_MIN_FREE_DISK_MIB", "0")
	f := &almacenFalso{}
	a := nuevoAlmacenFalso(t, m.root, f)
	m.alm = a
	a.mu.Lock()
	if err := a.preparar(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	a.mu.Unlock()
	id := newID()
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(a.dirInstancia(id), 0o755); err != nil {
		t.Fatal(err)
	}
	acum := a.acumuladoDiff(id)
	escribirPaginas(t, acum, paginaDe('d'))
	if err := os.Symlink(acum, filepath.Join(dir, "mem.file")); err != nil {
		t.Fatal(err)
	}
	falso := nuevoFcFalso(t)
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.MemMiB = 1 // sin DiffBase: volcado completo
	m.socket[id] = falso.Sock
	m.mu.Unlock()
	falso.enGancho(func(metodo, ruta string) {
		if metodo == http.MethodPut && ruta == "/snapshot/create" {
			_ = os.WriteFile(filepath.Join(dir, "snap.file"), []byte("snap"), 0o644)
			escribirPaginas(t, filepath.Join(dir, "mem.file"), paginaDe('A'))
		}
	})
	if _, err := m.Freeze(context.Background(), id); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if fi, err := os.Lstat(filepath.Join(dir, "mem.file")); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("el volcado completo no es un fichero propio: %v %v", fi, err)
	}
	if existe(acum) {
		t.Fatal("el acumulado del almacén sigue ahí (o el volcado se escribió dentro del almacén)")
	}
}
