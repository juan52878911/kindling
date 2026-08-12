package machine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/internal/api"
)

// write deja un fichero de n bytes dentro del directorio de una máquina.
func writeMachineFile(t *testing.T, m *Manager, id, name string, n int) {
	t.Helper()
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

// List devuelve el valor cacheado y NO recorre el disco. Es lo que evita un walk
// por máquina en cada `kling ps`, con el candado global cogido.
func TestListUsaLaCacheYNoElDisco(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	m.byID["a"] = &api.Machine{ID: "a", Name: "a", State: api.StateRunning, DiskBytes: 4242}
	m.mu.Unlock()

	// El directorio ni siquiera existe: si List recorriera el disco, daría 0.
	got := m.List()
	if len(got) != 1 || got[0].DiskBytes != 4242 {
		t.Fatalf("List no devolvió el valor cacheado: %+v", got)
	}
}

// El overlay CRECE mientras el invitado escribe. Un valor que solo se calculara
// al arrancar sería justo el que no sirve para ver a un invitado llenando disco.
func TestRefreshDiskUsageVeElCrecimiento(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	m.byID["a"] = &api.Machine{ID: "a", Name: "a", State: api.StateRunning}
	m.mu.Unlock()

	writeMachineFile(t, m, "a", "overlay.ext4", 64<<10)
	m.refreshDiskUsage()
	first := m.List()[0].DiskBytes
	if first == 0 {
		t.Fatal("no vio el fichero inicial")
	}

	writeMachineFile(t, m, "a", "overlay.ext4", 512<<10)
	m.refreshDiskUsage()
	if second := m.List()[0].DiskBytes; second <= first {
		t.Errorf("no vio crecer el overlay: %d -> %d", first, second)
	}
}

// touchDisk devuelve el valor para que la respuesta de Run/Freeze/Thaw lo lleve
// ya, en vez de un 0 hasta el siguiente tic del vigilante.
func TestTouchDiskDevuelveYGuarda(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	m.byID["a"] = &api.Machine{ID: "a", Name: "a", State: api.StateRunning}
	m.mu.Unlock()
	writeMachineFile(t, m, "a", "overlay.ext4", 128<<10)

	n := m.touchDisk("a")
	if n == 0 {
		t.Fatal("touchDisk devolvió 0 con un fichero de 128 KiB")
	}
	if cached := m.List()[0].DiskBytes; cached != n {
		t.Errorf("lo devuelto (%d) y lo cacheado (%d) no coinciden", n, cached)
	}
}

// Una máquina que desaparece del mapa mientras se cuenta no debe romper nada.
func TestRefreshDiskUsageToleraQueDesaparezca(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	m.byID["fugaz"] = &api.Machine{ID: "fugaz", Name: "fugaz"}
	m.mu.Unlock()

	go func() {
		m.mu.Lock()
		delete(m.byID, "fugaz")
		m.mu.Unlock()
	}()
	m.refreshDiskUsage() // no debe entrar en pánico
}

// La plantilla se construye en un .tmp y se renombra. Si existiera a medias con
// el nombre bueno, se copiaría rota a TODAS las microVMs para siempre.
func TestPlantillaDeOverlayNoDejaResiduos(t *testing.T) {
	m := newTestManager(t)
	if err := os.MkdirAll(filepath.Join(m.root, "images"), 0o755); err != nil {
		t.Fatal(err)
	}

	// mkfs.ext4 no existe en macOS, así que aquí solo se comprueba el contrato
	// que sí es independiente del sistema: o queda la plantilla completa, o no
	// queda nada — nunca un .tmp suelto ni un fichero a medias con el nombre
	// definitivo.
	err := m.ensureOverlayTemplate(t.Context())

	if _, statErr := os.Stat(m.overlayTemplatePath() + ".tmp"); statErr == nil {
		t.Error("quedó un .tmp suelto: un fallo a medias debe limpiarse")
	}
	if err != nil {
		if _, statErr := os.Stat(m.overlayTemplatePath()); statErr == nil {
			t.Error("falló pero dejó la plantilla con el nombre definitivo")
		}
		t.Skipf("no hay mkfs.ext4 en esta máquina: %v", err)
	}
	if _, statErr := os.Stat(m.overlayTemplatePath()); statErr != nil {
		t.Error("dijo que fue bien pero no dejó plantilla")
	}
}

// Llamarla dos veces no debe rehacerla ni fallar.
func TestPlantillaDeOverlayEsIdempotente(t *testing.T) {
	m := newTestManager(t)
	if err := os.MkdirAll(filepath.Join(m.root, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.overlayTemplatePath(), []byte("ya estaba"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.ensureOverlayTemplate(t.Context()); err != nil {
		t.Fatalf("con la plantilla ya presente no debería hacer nada: %v", err)
	}
	b, _ := os.ReadFile(m.overlayTemplatePath())
	if string(b) != "ya estaba" {
		t.Error("rehízo una plantilla que ya existía")
	}
}
