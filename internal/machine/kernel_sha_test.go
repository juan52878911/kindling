package machine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Pruebas de K2: kernel_sha256 en meta.json. runFrom se niega a restaurar un
// snapshot congelado con un vmlinux distinto del instalado ahora, con un
// error claro; los snapshots anteriores a este campo (sin él) siguen
// restaurándose. Y la firma de snapshots (firma.go) no lo cubre a propósito.

// escribirKernel deja un vmlinux de prueba en <root>/images/vmlinux y
// devuelve su sha256.
func escribirKernel(t *testing.T, m *Manager, contenido string) string {
	t.Helper()
	dir := filepath.Join(m.root, "images")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.KernelPath(), []byte(contenido), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := fileSHA256(m.KernelPath())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// kernelHash cachea por tamaño+fecha del vmlinux, igual que verifyIntegrity
// cachea el veredicto de un dorado: leerlo entero en cada Commit y cada
// runFrom sería justo el coste que el proyecto existe para evitar.
func TestKernelHashCacheaPorTamañoYFecha(t *testing.T) {
	m := newTestManager(t)
	h1 := escribirKernel(t, m, "kernel v1")

	got, err := m.kernelHash()
	if err != nil {
		t.Fatal(err)
	}
	if got != h1 {
		t.Fatalf("kernelHash = %s, quería %s", got, h1)
	}

	// Se pisa el fichero con OTRO contenido pero se le devuelve la MISMA huella
	// (tamaño y fecha): si de verdad está cacheado, kernelHash sigue devolviendo
	// el hash viejo sin volver a leer el fichero.
	st, err := os.Stat(m.KernelPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.KernelPath(), []byte("kernel v2 (mas largo)"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(m.KernelPath(), st.Size()); err != nil {
		t.Fatal(err)
	}
	// Truncate toca la fecha: Chtimes va DESPUÉS, o la huella no cuadraría.
	if err := os.Chtimes(m.KernelPath(), st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if got, err := m.kernelHash(); err != nil || got != h1 {
		t.Fatalf("con la misma huella, kernelHash = %s, %v; quería el cacheado %s sin releer", got, err, h1)
	}

	// Con una huella DISTINTA (cambia el tamaño), se rehashea y se detecta el
	// cambio real.
	time.Sleep(10 * time.Millisecond) // que la fecha pueda diferir en sistemas de baja resolución
	h2 := escribirKernel(t, m, "kernel v2, de verdad distinto")
	if h2 == h1 {
		t.Fatal("los dos contenidos de prueba dieron el mismo hash; ajustar la prueba")
	}
	got, err = m.kernelHash()
	if err != nil {
		t.Fatal(err)
	}
	if got != h2 {
		t.Fatalf("tras cambiar el vmlinux, kernelHash = %s, quería el nuevo %s", got, h2)
	}
}

// comprobarKernel: sin hash grabado (snapshot legacy) se acepta siempre, con
// el hash correcto se acepta, y con el hash equivocado se niega con un error
// que dice qué hacer.
func TestComprobarKernel(t *testing.T) {
	m := newTestManager(t)
	h := escribirKernel(t, m, "el kernel de este host")

	if err := m.comprobarKernel("", "svc"); err != nil {
		t.Errorf("sin hash grabado (legacy) debería aceptarse: %v", err)
	}
	if err := m.comprobarKernel(h, "svc"); err != nil {
		t.Errorf("con el hash correcto debería aceptarse: %v", err)
	}
	err := m.comprobarKernel(strings.Repeat("f", 64), "svc")
	if err == nil {
		t.Fatal("con el hash equivocado debería negarse")
	}
	if !strings.Contains(err.Error(), "rebuild the template") || !strings.Contains(err.Error(), "kernel changed") {
		t.Errorf("mensaje de error = %q, quería mencionar rebuild the template / kernel changed", err.Error())
	}
}

// Commit graba el kernel_sha256 del vmlinux en uso al congelar. Mismo arnés
// que TestCommitCompletoDejaSnapshotYPlantillaEnMarcha en commit_test.go.
func TestCommitGrabaElKernelSHA256(t *testing.T) {
	if _, err := exec.LookPath("fallocate"); err != nil {
		t.Skip("sin fallocate no se puede perforar el volcado")
	}
	m := newTestManager(t)
	id := "c2aa170000000001"
	falso, _ := plantillaParaCommit(t, m, id)
	kh := escribirKernel(t, m, "kernel de esta prueba")

	dir := m.snapDir("dorado")
	falso.enGancho(func(metodo, ruta string) {
		if ruta == "/snapshot/create" {
			// Lo que escribiría Firecracker.
			_ = os.WriteFile(filepath.Join(dir, "snap.file"), []byte("estado"), 0o644)
			_ = os.WriteFile(filepath.Join(dir, "mem.file"), make([]byte, 1<<20), 0o644)
		}
	})

	snap, err := m.Commit(context.Background(), id, "dorado", false)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if snap.KernelSHA256 != kh {
		t.Errorf("snap.KernelSHA256 = %q, quería %q", snap.KernelSHA256, kh)
	}
	// Y lo que queda en disco (loadSnapshot, sin cachés en memoria) lo lleva
	// también.
	got, err := m.loadSnapshot("dorado")
	if err != nil {
		t.Fatal(err)
	}
	if got.KernelSHA256 != kh {
		t.Errorf("meta.json en disco: kernel_sha256 = %q, quería %q", got.KernelSHA256, kh)
	}
}

// runFrom se niega a restaurar un snapshot cuyo kernel_sha256 no coincide con
// el vmlinux instalado ahora, y lo hace ANTES de tocar el disco (ni siquiera
// llega a copiar el overlay dorado).
func TestRunFromSeNiegaSiElKernelCambio(t *testing.T) {
	t.Setenv("KLING_REQUIRE_SIGNED", "")
	m := newTestManager(t)
	escribirKernel(t, m, "kernel nuevo, tras reconstruirlo con K1")

	// Legacy en digests (sin RootfsSHA256/SnapSHA256): así verifyIntegrity se
	// salta sin necesitar overlay.ext4/snap.file de verdad en disco, y lo único
	// que se ejercita es la comprobación del kernel.
	escribirSnapshot(t, m, "dorado", api.Snapshot{
		KernelSHA256: strings.Repeat("a", 64), // el kernel con el que se congeló, ya no es el de hoy
	})

	_, err := m.runFrom(context.Background(), api.RunRequest{From: "dorado"})
	if err == nil {
		t.Fatal("runFrom con el kernel cambiado no devolvió error")
	}
	if !strings.Contains(err.Error(), "rebuild the template") {
		t.Errorf("runFrom con el kernel cambiado: %v, quería 'rebuild the template'", err)
	}
	// No debe haber creado el directorio de la instancia: se niega antes de
	// copiar nada.
	m.mu.RLock()
	n := len(m.byID)
	m.mu.RUnlock()
	if n != 0 {
		t.Errorf("runFrom rechazado por el kernel dejó %d máquina(s) registradas", n)
	}
	// Y suelta su reserva del snapshot al salir.
	m.mu.RLock()
	_, queda := m.reserved[reservaSnapshot("dorado")]
	m.mu.RUnlock()
	if queda {
		t.Error("runFrom rechazado por el kernel no soltó la reserva del snapshot")
	}
}

// Un snapshot SIN kernel_sha256 (anterior a K2, o congelado antes de K1) sigue
// restaurándose: la comprobación se salta, no falla.
func TestRunFromAceptaSnapshotSinKernelSHA256(t *testing.T) {
	t.Setenv("KLING_REQUIRE_SIGNED", "")
	m := newTestManager(t)
	escribirKernel(t, m, "kernel actual, da igual cuál")

	escribirSnapshot(t, m, "legacy", api.Snapshot{
		RootfsSHA256: strings.Repeat("a", 64),
		SnapSHA256:   strings.Repeat("b", 64),
	})

	_, err := m.runFrom(context.Background(), api.RunRequest{From: "legacy"})
	// Sin overlay.ext4/snap.file reales, el fallo de integridad SÍ debe
	// ocurrir (eso no lo salta el legacy de kernel) — lo que este test
	// exige es que NO sea el error de kernel.
	if err == nil {
		t.Fatal("se esperaba un error (integridad), pero no por el kernel")
	}
	if strings.Contains(err.Error(), "rebuild the template") {
		t.Errorf("un snapshot sin kernel_sha256 no debería fallar por el kernel: %v", err)
	}
}
