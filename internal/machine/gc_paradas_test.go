package machine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// Las paradas también se recogen, pero solo las que no pierden nada: la
// instancia de un servicio sale de su dorado y se recrea igual. Una arrancada
// en frío o una copia sin servicio (una rama de kling db) guardan en su
// overlay lo que escribieron, y kling start las vuelve a arrancar. Una parada
// sin fecha (de antes del campo) recibe su reloj y no se toca aún.
func TestGCRecogeSoloLasParadasQueSeRecrean(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	vieja := time.Now().Add(-2 * defaultStoppedRetention)
	reciente := time.Now().Add(-time.Minute)
	svc := map[string]string{api.LabelService: "web"}

	deServicio, deServicioReciente := "0b0b000000000001", "0b0b000000000002"
	enFrio, copiaDB, sinFecha := "0b0b000000000003", "0b0b000000000004", "0b0b000000000005"
	m.mu.Lock()
	m.byID[deServicio] = &api.Machine{ID: deServicio, Name: "web-1", State: api.StateStopped, From: "web", Labels: svc, StoppedAt: &vieja}
	m.byID[deServicioReciente] = &api.Machine{ID: deServicioReciente, Name: "web-2", State: api.StateStopped, From: "web", Labels: svc, StoppedAt: &reciente}
	m.byID[enFrio] = &api.Machine{ID: enFrio, Name: "frio", State: api.StateStopped, StoppedAt: &vieja}
	m.byID[copiaDB] = &api.Machine{ID: copiaDB, Name: "rama", State: api.StateStopped, From: "pg", StoppedAt: &vieja}
	m.byID[sinFecha] = &api.Machine{ID: sinFecha, Name: "web-3", State: api.StateStopped, From: "web", Labels: svc}
	m.mu.Unlock()

	m.gcFailed()

	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, sigue := m.byID[deServicio]; sigue {
		t.Error("no recogió la instancia parada de un servicio pasada su retención")
	}
	for _, id := range []string{deServicioReciente, enFrio, copiaDB, sinFecha} {
		if _, sigue := m.byID[id]; !sigue {
			t.Errorf("recogió %s, que no tocaba", id)
		}
	}
	if m.byID[sinFecha].StoppedAt == nil {
		t.Error("la parada sin fecha no recibió su reloj")
	}
}

// gcFailed decide con una foto y borra fuera del candado: si entre medias
// alguien arranca la máquina (kling start, con su cerrojo), no se borra.
func TestGCNoBorraLaParadaQueArrancaronEntreMedias(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	t.Setenv("KLING_STOPPED_RETENTION", "1ms")
	id := "0b0b000000000010"
	vieja := time.Now().Add(-time.Hour)
	m.mu.Lock()
	m.byID[id] = &api.Machine{ID: id, Name: "web-9", State: api.StateStopped, From: "web",
		Labels: map[string]string{api.LabelService: "web"}, StoppedAt: &vieja}
	m.mu.Unlock()

	soltar := m.lock(id) // el arranque la tiene
	hecho := make(chan struct{})
	go func() {
		m.gcFailed()
		close(hecho)
	}()
	esperarQueEspere(t, m, id)
	m.mu.Lock()
	m.byID[id].State = api.StateRunning
	m.byID[id].StoppedAt = nil
	m.mu.Unlock()
	soltar()
	<-hecho

	if got := vivaDe(t, m, id).State; got != api.StateRunning {
		t.Fatalf("estado = %s; el GC borró o tocó una máquina que ya corría", got)
	}
}

// fifoQueEspera deja en path una tubería con nombre: quien la abra para leer
// se queda esperando hasta que el test abra el otro extremo (soltar).
func fifoQueEspera(t *testing.T, path string) (soltar func()) {
	t.Helper()
	_ = os.Remove(path)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("sin mkfifo: %v", err)
	}
	return func() {
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err == nil {
			_, _ = f.Write([]byte("x"))
			f.Close()
		}
	}
}

// esperarCandadoLibre comprueba, mientras la recogida está bloqueada leyendo
// el disco, que m.mu sigue libre para los demás.
func esperarCandadoLibre(t *testing.T, m *Manager, hecho <-chan struct{}) {
	t.Helper()
	// Lo bastante para que la recogida llegue a la lectura bloqueada.
	time.Sleep(100 * time.Millisecond)
	select {
	case <-hecho:
		t.Fatal("la recogida terminó sin llegar a leer el disco")
	default:
	}
	if !m.mu.TryLock() {
		t.Fatal("m.mu está tomado mientras la recogida lee el disco: para a todo el daemon")
	}
	m.mu.Unlock()
}

// gcFailed hacía el sha256 del volcado de cada failed (retieneDatos) con
// m.mu tomado en escritura, cada diez segundos: un disco lento paraba todos
// los run, ps y thaw del daemon. Ahora lee fuera del candado.
func TestGCFailedLeeElDiscoSinElCandado(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "0b0b000000000020"
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mem.file"), []byte("memoria"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(sello{SnapSHA256: "x", MemBytes: int64(len("memoria"))})
	if err := os.WriteFile(filepath.Join(dir, marcaOK), b, 0o600); err != nil {
		t.Fatal(err)
	}
	soltar := fifoQueEspera(t, filepath.Join(dir, "snap.file"))
	vieja := time.Now().Add(-2 * defaultFailedRetention)
	m.mu.Lock()
	m.byID[id] = &api.Machine{ID: id, Name: "rota", State: api.StateFailed, From: "svc", FailedAt: &vieja}
	m.mu.Unlock()

	hecho := make(chan struct{})
	go func() {
		m.gcFailed()
		close(hecho)
	}()
	esperarCandadoLibre(t, m, hecho)
	soltar()
	<-hecho
}

// gcDisk leía el meta.json del dorado de cada candidata (loadSnapshot, sin
// caché) con m.mu tomado: lo mismo, con el disco lleno, que es justo cuando
// corre.
func TestGCDiskLeeElDoradoSinElCandado(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	t.Setenv("KLING_GC_DISK_HIGH", "1")
	t.Setenv("KLING_GC_DISK_TARGET", "1")
	if p := m.diskUsedPct(); p < 1 {
		t.Skipf("el disco de pruebas está al %d %%", p)
	}
	if err := os.MkdirAll(m.snapDir("web"), 0o755); err != nil {
		t.Fatal(err)
	}
	soltar := fifoQueEspera(t, filepath.Join(m.snapDir("web"), "meta.json"))
	id := "0b0b000000000030"
	congelada := time.Now().Add(-time.Hour)
	m.mu.Lock()
	m.byID[id] = &api.Machine{ID: id, Name: "web-1", State: api.StateWarm, From: "web",
		Labels: map[string]string{api.LabelService: "web"}, FrozenAt: &congelada}
	m.mu.Unlock()

	hecho := make(chan struct{})
	go func() {
		m.gcDisk(t.Context())
		close(hecho)
	}()
	esperarCandadoLibre(t, m, hecho)
	soltar()
	<-hecho
}
