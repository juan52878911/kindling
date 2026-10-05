package machine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Un volcado que no cabe se rechaza ANTES de pausar, con el mismo código que
// el disco lleno de la admisión: la RAM entera más el mínimo de siempre, para
// que el host siga admitiendo máquinas después del volcado.
func TestVolcadoQueNoCabeSeRechazaAntes(t *testing.T) {
	m := newTestManager(t)
	t.Setenv("KLING_MIN_FREE_DISK_MIB", "0")
	if err := m.checkDiskParaVolcado(0, "freeze"); err != nil {
		t.Fatalf("sin RAM que volcar ni mínimo: %v", err)
	}
	// Más RAM de la que puede haber libre en ningún disco.
	err := m.checkDiskParaVolcado(1<<40, "freeze")
	if err == nil {
		t.Fatal("un volcado de 1 PiB tenía que rechazarse")
	}
	if !api.IsDiskFull(err) {
		t.Fatalf("no se reconoce como disco lleno: %v", err)
	}
	if api.IsInsufficientMemory(err) {
		t.Fatal("se confunde con falta de memoria: el planificador congelaría para hacer sitio")
	}
	// El mínimo del daemon cuenta aunque la RAM sea poca.
	t.Setenv("KLING_MIN_FREE_DISK_MIB", "999999999")
	if err := m.checkDiskParaVolcado(1, "save"); err == nil || !api.IsDiskFull(err) {
		t.Fatalf("el mínimo del daemon no se suma al volcado: %v", err)
	}
}

// Lo que un volcado fallido dejó a medias se borra: el mem.file del tamaño de
// la RAM y la marca de en curso. La máquina sigue running y nadie más lo haría.
func TestVolcadoFallidoNoDejaRestos(t *testing.T) {
	m := newTestManager(t)
	id := "abcdef0123456789"
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"snap.file", "mem.file"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("a medias"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := volcadoEnCurso(dir); err != nil {
		t.Fatal(err)
	}
	m.borrarVolcadoParcial(id, false, dir)
	for _, f := range []string{"snap.file", "mem.file", marcaEnCurso} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Fatalf("%s sigue ahí tras un volcado fallido", f)
		}
	}
	// Sin restos tampoco es un error.
	m.borrarVolcadoParcial(id, false, dir)
}
