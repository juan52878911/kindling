package machine

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Un volcado que no cabe se rechaza ANTES de pausar, con el mismo código que
// el disco lleno de la admisión: la RAM entera más el mínimo de siempre, para
// que el host siga admitiendo máquinas después del volcado.
func TestVolcadoQueNoCabeSeRechazaAntes(t *testing.T) {
	m := newTestManager(t)
	t.Setenv("KLING_MIN_FREE_DISK_MIB", "0")
	soltar, err := m.reservarDiscoParaVolcado(0, "freeze")
	if err != nil {
		t.Fatalf("sin RAM que volcar ni mínimo: %v", err)
	}
	soltar()
	// Más RAM de la que puede haber libre en ningún disco.
	_, err = m.reservarDiscoParaVolcado(1<<40, "freeze")
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
	if _, err := m.reservarDiscoParaVolcado(1, "save"); err == nil || !api.IsDiskFull(err) {
		t.Fatalf("el mínimo del daemon no se suma al volcado: %v", err)
	}
}

// discoLibreFalso hace que statfsRaiz diga que quedan libreMiB.
func discoLibreFalso(t *testing.T, libreMiB uint64) {
	t.Helper()
	old := statfsRaiz
	statfsRaiz = func(_ string, st *syscall.Statfs_t) error {
		st.Bsize = 1 << 20
		st.Bavail = libreMiB
		st.Blocks = libreMiB * 4
		return nil
	}
	t.Cleanup(func() { statfsRaiz = old })
}

// Comprobar y actuar: N freezes a la vez miraban el mismo disco libre y
// pasaban todos aunque juntos no cupieran. Lo que reservó uno y aún no ha
// escrito cuenta para el siguiente, y se devuelve al soltarlo.
func TestVolcadosSimultaneosNoPasanTodos(t *testing.T) {
	m := newTestManager(t)
	t.Setenv("KLING_MIN_FREE_DISK_MIB", "100")
	// Caben dos de 400 (más el mínimo), no tres.
	discoLibreFalso(t, 1000)

	const n = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		soltar  []func()
		negados int
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := m.reservarDiscoParaVolcado(400, "freeze")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if !api.IsDiskFull(err) {
					t.Errorf("error que no es disco lleno: %v", err)
				}
				negados++
				return
			}
			soltar = append(soltar, s)
		}()
	}
	wg.Wait()
	if len(soltar) != 2 || negados != n-2 {
		t.Fatalf("admitidos %d, negados %d; quería 2 y %d", len(soltar), negados, n-2)
	}
	_, err := m.reservarDiscoParaVolcado(400, "freeze")
	if err == nil || !strings.Contains(err.Error(), "800 MiB of it reserved") {
		t.Fatalf("el rechazo no cuenta lo reservado por los demás: %v", err)
	}
	for _, s := range soltar {
		s()
		s() // soltar dos veces no devuelve de más
	}
	m.mu.RLock()
	quedan := m.volcandoMiB
	m.mu.RUnlock()
	if quedan != 0 {
		t.Fatalf("volcandoMiB = %d tras soltarlo todo", quedan)
	}
	s, err := m.reservarDiscoParaVolcado(400, "freeze")
	if err != nil {
		t.Fatalf("con todo soltado no cabe uno: %v", err)
	}
	s()
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

// Los tres rechazos por disco lleno dicen qué borrar con los estados que kling
// ps muestra hoy: "warm" era de v0.13 y ya no existe.
func TestDiscoLlenoNoNombraEstadosViejos(t *testing.T) {
	m := newTestManager(t)
	t.Setenv("KLING_MIN_FREE_DISK_MIB", "100")
	discoLibreFalso(t, 10)
	errs := map[string]error{"admisión": m.checkDisk()}
	_, errs["freeze"] = m.reservarDiscoParaVolcado(400, "freeze")
	_, errs["thaw"] = m.reservarDiscoParaVolcado(400, "thaw")
	for que, err := range errs {
		if !api.IsDiskFull(err) {
			t.Fatalf("%s: no es disco lleno: %v", que, err)
		}
		if msg := err.Error(); strings.Contains(msg, "warm") || !strings.Contains(msg, "frozen") {
			t.Errorf("%s: el consejo no nombra los estados de hoy: %s", que, msg)
		}
	}
}
