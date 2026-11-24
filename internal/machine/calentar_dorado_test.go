package machine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// doradoParaCalentar deja un dorado name con un mem.file de mib MiB con datos
// y devuelve la ruta del mem.file.
func doradoParaCalentar(t *testing.T, m *Manager, name string, mib int) string {
	t.Helper()
	writeSnapMeta(t, m, name, `{"name": "`+name+`", "image": "eco", "created_at": "2026-11-23T10:00:00Z", "vcpus": 1, "mem_mib": 256}`)
	mem := filepath.Join(m.snapDir(name), "mem.file")
	b := make([]byte, mib<<20)
	for i := range b {
		b[i] = byte(i>>12 + 1)
	}
	if err := os.WriteFile(mem, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return mem
}

// POST /snapshots/{name}/warm con wait deja el mem.file entero en la caché
// (mincore), y repetirlo con todo ya en caché no lanza otra lectura.
func TestCalentarDoradoDejaTodoEnCache(t *testing.T) {
	m := annotTestManager(t)
	mem := doradoParaCalentar(t, m, "modelo", 24)
	dropCache(mem) // en Linux, frío de verdad; en macOS no hace nada
	res, err := m.CalentarDorado(context.Background(), "modelo", api.WarmSnapshotRequest{Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Bytes == 0 || res.CachedBytes < res.Bytes {
		t.Errorf("tras calentar con wait: %d de %d bytes en caché", res.CachedBytes, res.Bytes)
	}
	if got := bytesEnCache(mem); got < res.Bytes {
		t.Errorf("mincore dice %d bytes en caché de %d", got, res.Bytes)
	}
	res, err = m.CalentarDorado(context.Background(), "modelo", api.WarmSnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Started {
		t.Error("con el dorado ya en caché se lanzó otra lectura")
	}
	if res.Pinned {
		t.Error("sin pin quedó anclado")
	}
}

func TestCalentarDoradoQueNoExiste(t *testing.T) {
	m := annotTestManager(t)
	if _, err := m.CalentarDorado(context.Background(), "nada", api.WarmSnapshotRequest{}); !errors.Is(err, ErrNoSnapshot) {
		t.Errorf("err = %v, quería ErrNoSnapshot", err)
	}
}

// precargarParalelo lee todos los tramos con datos, con varios lectores, y
// nada más.
func TestPrecargarParaleloLeeTodo(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mem")
	b := make([]byte, 21<<20+123)
	for i := range b {
		b[i] = 1
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if n := precargarParalelo(context.Background(), p, 6); n != int64(len(b)) {
		t.Errorf("leyó %d bytes de %d", n, len(b))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n := precargarParalelo(ctx, p, 6); n != 0 {
		t.Errorf("con el contexto cancelado leyó %d bytes", n)
	}
}

// runFromUna precarga el dorado si está frío, y no si ya está en caché; dos
// a la vez del mismo dorado no lanzan dos lecturas.
func TestRunFromPrecargaSiFrio(t *testing.T) {
	m := annotTestManager(t)
	mem := doradoParaCalentar(t, m, "modelo", 8)
	precargarParalelo(context.Background(), mem, 4)
	if m.precargarDoradoSiFrio(mem) {
		t.Error("con el dorado en caché se lanzó la precarga")
	}
	if runtime.GOOS != "linux" {
		t.Skip("sin POSIX_FADV_DONTNEED no se puede enfriar el fichero")
	}
	dropCache(mem)
	if bytesEnCache(mem) >= allocatedBytes(mem)/2 {
		t.Skip("el kernel no soltó la caché del fichero")
	}
	if !m.precargarDoradoSiFrio(mem) {
		t.Error("con el dorado frío no se lanzó la precarga")
	}
	m.sueltas.Wait()
	if got := bytesEnCache(mem); got < allocatedBytes(mem) {
		t.Errorf("tras la precarga, %d de %d bytes en caché", got, allocatedBytes(mem))
	}
}

// anclajeFalso cuenta lo anclado y si se soltó.
type anclajeFalso struct {
	n       int64
	soltado *atomic.Bool
}

func (a anclajeFalso) bytes() int64 { return a.n }
func (a anclajeFalso) soltar()      { a.soltado.Store(true) }

// Anclar pasa por la admisión de memoria con lo que va a anclar: sin sitio,
// 507 y no se ancla nada; con sitio se ancla, la reserva se devuelve al
// acabar (las páginas ancladas ya no cuentan como disponibles para el kernel),
// y Pin=false, borrar el dorado o cerrar el Manager lo sueltan.
func TestAncladoCuentaEnAdmision(t *testing.T) {
	m := annotTestManager(t)
	doradoParaCalentar(t, m, "modelo", 4)
	var llamadas atomic.Int32
	var soltado atomic.Bool
	antes, antesAvail := anclarFichero, availableMiB
	t.Cleanup(func() { anclarFichero, availableMiB = antes, antesAvail })
	anclarFichero = func(path string) (anclaje, error) {
		llamadas.Add(1)
		return anclajeFalso{n: allocatedBytes(path), soltado: &soltado}, nil
	}
	si := true

	availableMiB = func() int { return 64 } // 4 MiB + reserva del host no caben
	_, err := m.CalentarDorado(context.Background(), "modelo", api.WarmSnapshotRequest{Pin: &si})
	var se *api.StatusError
	if !errors.As(err, &se) || se.Code != api.StatusInsufficientMemory {
		t.Fatalf("sin memoria: err = %v, quería 507", err)
	}
	if llamadas.Load() != 0 || m.estaAnclado("modelo") {
		t.Fatal("sin memoria se ancló igual")
	}

	availableMiB = func() int { return 1 << 20 }
	res, err := m.CalentarDorado(context.Background(), "modelo", api.WarmSnapshotRequest{Pin: &si})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pinned || llamadas.Load() != 1 {
		t.Fatalf("pinned=%v llamadas=%d", res.Pinned, llamadas.Load())
	}
	m.mu.Lock()
	pend := m.pendingMiB
	m.mu.Unlock()
	if pend != 0 {
		t.Errorf("la reserva de la admisión se quedó: pendingMiB=%d", pend)
	}
	// Idempotente: anclar otra vez no vuelve a anclar.
	if _, err := m.CalentarDorado(context.Background(), "modelo", api.WarmSnapshotRequest{Pin: &si}); err != nil || llamadas.Load() != 1 {
		t.Errorf("anclar dos veces: err=%v llamadas=%d", err, llamadas.Load())
	}
	no := false
	res, err = m.CalentarDorado(context.Background(), "modelo", api.WarmSnapshotRequest{Pin: &no})
	if err != nil || res.Pinned || !soltado.Load() {
		t.Errorf("Pin=false: err=%v pinned=%v soltado=%v", err, res.Pinned, soltado.Load())
	}

	// Borrar el dorado lo suelta.
	soltado.Store(false)
	if _, err := m.CalentarDorado(context.Background(), "modelo", api.WarmSnapshotRequest{Pin: &si}); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveSnapshot("modelo"); err != nil {
		t.Fatal(err)
	}
	if !soltado.Load() || m.estaAnclado("modelo") {
		t.Error("borrar el dorado no soltó el anclado")
	}
}

// Donde no se puede anclar (macOS), anclar es ErrAncladoNoSoportado (501 en
// el daemon), sin pasar por la admisión ni tocar nada.
func TestAnclarSinSoporte(t *testing.T) {
	m := annotTestManager(t)
	doradoParaCalentar(t, m, "modelo", 1)
	antes := anclarFichero
	anclarFichero = nil
	t.Cleanup(func() { anclarFichero = antes })
	si := true
	if _, err := m.CalentarDorado(context.Background(), "modelo", api.WarmSnapshotRequest{Pin: &si}); !errors.Is(err, ErrAncladoNoSoportado) {
		t.Errorf("err = %v, quería ErrAncladoNoSoportado", err)
	}
}

// En Linux el anclado de verdad: mlock de los tramos con datos de un fichero
// pequeño (cabe en el RLIMIT_MEMLOCK de un usuario sin privilegios).
func TestAnclarFicheroLinux(t *testing.T) {
	if anclarFicheroPlataforma == nil {
		t.Skip("sin anclado en esta plataforma")
	}
	p := filepath.Join(t.TempDir(), "mem")
	if err := os.WriteFile(p, make([]byte, 256<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := anclarFicheroPlataforma(p)
	if err != nil {
		t.Skipf("mlock no disponible aquí: %v", err)
	}
	defer a.soltar()
	if a.bytes() <= 0 || a.bytes() > 256<<10 {
		t.Errorf("anclados %d bytes de 256 KiB", a.bytes())
	}
	if bytesEnCache(p) < 256<<10 {
		t.Errorf("anclado y no está en caché: %d bytes", bytesEnCache(p))
	}
}
