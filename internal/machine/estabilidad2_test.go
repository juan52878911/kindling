package machine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func escribir(t *testing.T, p, contenido string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(contenido), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Un volcado se da por bueno solo si se selló. Los casos son los que se ven en
// un host de verdad tras un corte a mitad de congelar.
func TestVolcadoValido(t *testing.T) {
	dir := t.TempDir()
	escribir(t, filepath.Join(dir, "snap.file"), "estado")
	escribir(t, filepath.Join(dir, "mem.file"), "memoria-entera")

	// Sin marcas: una máquina congelada antes de que existieran. Se acepta.
	if err := volcadoValido(dir); err != nil {
		t.Fatalf("anterior a los sellos: %v", err)
	}

	// Empezó a volcar y no terminó.
	if err := volcadoEnCurso(dir); err != nil {
		t.Fatal(err)
	}
	if err := volcadoValido(dir); !errors.Is(err, errVolcadoIncompleto) {
		t.Fatalf("volcado interrumpido: %v, quería errVolcadoIncompleto", err)
	}

	// Terminó: vale, y la marca de en curso desaparece.
	if err := sellarVolcado(dir); err != nil {
		t.Fatal(err)
	}
	if err := volcadoValido(dir); err != nil {
		t.Fatalf("sellado: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, marcaEnCurso)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("la marca de volcado en curso sigue ahí tras sellar")
	}

	// La memoria quedó más corta que al sellar: truncada.
	escribir(t, filepath.Join(dir, "mem.file"), "memoria")
	if err := volcadoValido(dir); !errors.Is(err, errVolcadoIncompleto) {
		t.Fatalf("memoria truncada: %v", err)
	}
	escribir(t, filepath.Join(dir, "mem.file"), "memoria-entera")

	// El estado cambió después de sellarlo.
	escribir(t, filepath.Join(dir, "snap.file"), "otro-estado")
	if err := volcadoValido(dir); !errors.Is(err, errVolcadoIncompleto) {
		t.Fatalf("estado cambiado: %v", err)
	}

	// Y empezar otro volcado invalida el sello anterior.
	escribir(t, filepath.Join(dir, "snap.file"), "estado")
	_ = sellarVolcado(dir)
	_ = volcadoEnCurso(dir)
	if err := volcadoValido(dir); err == nil {
		t.Fatal("un volcado nuevo a medias no puede heredar el sello del anterior")
	}
}

// reconcile no puede declarar warm una máquina con el volcado a medias.
func TestHasSnapshotExigeElSello(t *testing.T) {
	m := newTestManager(t)
	id := "5e11000000000001"
	escribir(t, filepath.Join(m.dir(id), "snap.file"), "s")
	escribir(t, filepath.Join(m.dir(id), "mem.file"), "m")
	if !m.hasSnapshot(id) {
		t.Fatal("sin marcas (máquina anterior) tenía que valer")
	}
	_ = volcadoEnCurso(m.dir(id))
	if m.hasSnapshot(id) {
		t.Fatal("con el volcado interrumpido la dio por congelada")
	}
}

// Un commit interrumpido deja un directorio sin meta.json. Antes no había forma
// de borrarlo más que a mano, y bloqueaba `commit -replace` del mismo nombre.
func TestRestosDeCommitSeBorranSalvoSiSeEstanEscribiendo(t *testing.T) {
	m := newTestManager(t)
	escribir(t, filepath.Join(m.snapDir("roto"), "mem.file"), "m")

	soltar := m.reserveDir(reservaSnapshot("roto"))
	if err := m.RemoveSnapshot("roto"); err == nil {
		t.Fatal("borró un snapshot que se estaba escribiendo")
	}
	// El propio commit que tiene la reserva sí puede (camino de -replace).
	escribir(t, filepath.Join(m.snapDir("roto"), "mem.file"), "m")
	if err := m.removeSnapshot("roto", true); err != nil {
		t.Fatalf("el commit dueño de la reserva no pudo reemplazar sus restos: %v", err)
	}
	soltar()

	escribir(t, filepath.Join(m.snapDir("roto2"), "mem.file"), "m")
	if err := m.RemoveSnapshot("roto2"); err != nil {
		t.Fatalf("restos sin nadie escribiéndolos: %v", err)
	}
	if _, err := os.Stat(m.snapDir("roto2")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("los restos siguen ahí")
	}
}

// El barrido de restos de commits los aparta a la papelera y la papelera se
// vacía fuera del cerrojo.
func TestBarridoDeRestosYPapelera(t *testing.T) {
	m := newTestManager(t)
	restos := m.snapDir("viejo")
	escribir(t, filepath.Join(restos, "mem.file"), "m")
	hace := time.Now().Add(-2 * dirGrace)
	_ = os.Chtimes(restos, hace, hace)

	// Uno recién empezado no se toca: puede ser un commit en marcha que aún no
	// reservó... o uno que acaba de morir; el siguiente barrido lo recogerá.
	escribir(t, filepath.Join(m.snapDir("nuevo"), "mem.file"), "m")

	m.mu.Lock()
	m.sweepSnapshotLeftovers()
	m.mu.Unlock()
	if _, err := os.Stat(restos); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("los restos viejos siguen en snapshots/")
	}
	if _, err := os.Stat(m.snapDir("nuevo")); err != nil {
		t.Fatal("se llevó unos restos recientes")
	}

	m.vaciarPapelera()
	entradas, _ := os.ReadDir(filepath.Join(m.root, "machines", papeleraDir))
	if len(entradas) != 0 {
		t.Fatalf("la papelera no se vació: %d entradas", len(entradas))
	}
}

// Con dos escritores de state.json, una foto vieja escrita tarde no puede pisar
// a una nueva.
func TestUnaFotoViejaNoPisaAUnaNueva(t *testing.T) {
	m := newTestManager(t)
	m.addForTest("a000000000000001")
	m.persistirYa() // generación N escrita
	escrito := m.escritoGen

	// Una foto anterior que llega tarde.
	m.stateMu.Lock()
	m.pending, m.hasPend, m.pendGen = nil, true, escrito-1
	m.stateMu.Unlock()
	m.writePending()
	if m.escritoGen != escrito {
		t.Fatalf("escribió una foto vieja (gen %d) encima de la %d", m.escritoGen, escrito)
	}
	b, _ := os.ReadFile(m.statePath())
	if len(b) < 10 {
		t.Fatalf("state.json quedó vacío: %q", b)
	}
}
