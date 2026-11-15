package machine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// overlayDe crea (o cambia de tamaño) el overlay de id, disperso.
func overlayDe(t *testing.T, m *Manager, id string, tam int64) {
	t.Helper()
	d := filepath.Join(m.root, "machines", id)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(d, "overlay.ext4"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(tam); err != nil {
		t.Fatal(err)
	}
}

func estadoVigia(m *Manager, id string) api.State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byID[id].State
}

// overlayDeDorado deja el overlay del dorado name, disperso, de tam bytes.
func overlayDeDorado(t *testing.T, m *Manager, name string, tam int64) {
	t.Helper()
	if err := os.MkdirAll(m.snapDir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.snapDir(name), "overlay.ext4"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(m.snapDir(name), "overlay.ext4"), tam); err != nil {
		t.Fatal(err)
	}
}

// Sin almacén no hay cuota del núcleo sobre el overlay: un VMM que lo alarga
// más allá de lo que su invitado puede direccionar se para (antes llenaba el
// disco del host). Lo normal, y lo que ya era mayor porque así vino de su
// dorado, no.
func TestVigiaOverlaysParaLaQueCrece(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	ahora := time.Now()
	logico := int64(defaultOverlayMiB) << 20
	overlayDeDorado(t, m, "gordo", 4*logico)
	for _, id := range []string{"normal", "inflada", "grande", "disco2g"} {
		mc := m.addForTest(id)
		m.mu.Lock()
		mc.StartedAt = &ahora
		switch id {
		case "disco2g":
			mc.DiskMiB = 2048
		case "grande":
			mc.From = "gordo" // de un daemon que no apuntaba DiskMiB
		}
		m.mu.Unlock()
	}
	overlayDe(t, m, "normal", logico)
	overlayDe(t, m, "inflada", logico)
	overlayDe(t, m, "grande", 4*logico) // ya venía así: se vigila desde ahí
	overlayDe(t, m, "disco2g", 2048<<20)

	if p := m.vueltaVigiaOverlays(); len(p) != 0 {
		t.Fatalf("primera vuelta paró %v; no quería ninguna", p)
	}

	overlayDe(t, m, "inflada", 64<<30) // ftruncate de un VMM comprometido
	overlayDe(t, m, "normal", logico+1<<20)
	if p := m.vueltaVigiaOverlays(); !slices.Equal(p, []string{"inflada"}) {
		t.Fatalf("paró %v; quería [inflada]", p)
	}
	if st := estadoVigia(m, "inflada"); st == api.StateRunning {
		t.Fatalf("la inflada sigue %s", st)
	}
	for _, id := range []string{"normal", "grande", "disco2g"} {
		if st := estadoVigia(m, id); st != api.StateRunning {
			t.Errorf("%s quedó %s; quería running", id, st)
		}
	}

	overlayDe(t, m, "grande", 8*logico)
	if p := m.vueltaVigiaOverlays(); !slices.Equal(p, []string{"grande"}) {
		t.Fatalf("la que ya era grande creció más y se paró %v; quería [grande]", p)
	}
}

// M1: el primer vistazo a una máquina sin tope apuntado (de un daemon de
// antes) no adopta lo que mide: su VMM ya pudo alargar el fichero. La línea
// base es su disco o el overlay de su dorado. Solo si el dorado ya no está se
// adopta lo que mide, para no parar lo que ya era así.
func TestVigiaOverlaysPrimerVistazoNoAdoptaLoInflado(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	ahora := time.Now()
	logico := int64(defaultOverlayMiB) << 20
	overlayDeDorado(t, m, "dorado", logico)
	for id, from := range map[string]string{"fria": "", "copia": "dorado", "huerfana": "borrado"} {
		mc := m.addForTest(id)
		m.mu.Lock()
		mc.StartedAt, mc.From = &ahora, from
		m.mu.Unlock()
	}
	overlayDe(t, m, "fria", 10<<30) // alargado antes del primer vistazo
	overlayDe(t, m, "copia", 10<<30)
	overlayDe(t, m, "huerfana", 4*logico)

	if p := m.vueltaVigiaOverlays(); !slices.Equal(p, []string{"copia", "fria"}) {
		t.Fatalf("primer vistazo paró %v; quería [copia fria]", p)
	}
	if st := estadoVigia(m, "huerfana"); st != api.StateRunning {
		t.Fatalf("la de un dorado borrado quedó %s; quería running", st)
	}
}

// R6: sin máquinas en marcha el vigilante no despierta cada segundo; un
// cambio de estado (persist) lo despierta y vuelve a su ritmo.
func TestVigiaOverlaysDuermeSinMaquinas(t *testing.T) {
	var vueltas atomic.Int32
	trasVueltaVigiaOverlays = func() { vueltas.Add(1) }
	t.Cleanup(func() { trasVueltaVigiaOverlays = nil })
	m := newTestManager(t)
	m.bus = events.New()
	ctx, cancel := context.WithCancel(context.Background())
	fin := make(chan struct{})
	go func() { m.vigilarOverlays(ctx); close(fin) }()
	defer func() { cancel(); <-fin }() // antes del Cleanup que quita el gancho

	time.Sleep(2500 * time.Millisecond)
	if n := vueltas.Load(); n != 0 {
		t.Fatalf("sin máquinas dio %d vuelta(s) en 2,5 s; quería 0", n)
	}

	// Una máquina que arranca con el overlay ya alargado: persist despierta
	// al vigilante y éste la para enseguida.
	ahora := time.Now()
	mc := m.addForTest("nueva")
	overlayDe(t, m, "nueva", 10<<30)
	m.mu.Lock()
	mc.StartedAt, mc.OverlayMaxBytes = &ahora, cuotaInstancia(int64(defaultOverlayMiB)<<20)
	m.persist()
	m.mu.Unlock()
	limite := time.Now().Add(3 * time.Second)
	for estadoVigia(m, "nueva") == api.StateRunning {
		if time.Now().After(limite) {
			t.Fatal("el aviso de persist no despertó al vigilante")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// El tope que se apunta al crear el disco: el de su disco lógico, o el de la
// copia del dorado si ya es mayor.
func TestTopeOverlayAlCrear(t *testing.T) {
	d := t.TempDir()
	ruta := filepath.Join(d, "overlay.ext4")
	if err := os.WriteFile(ruta, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	logico := int64(defaultOverlayMiB) << 20
	if err := os.Truncate(ruta, logico); err != nil {
		t.Fatal(err)
	}
	if got := topeOverlayAlCrear(ruta, 0); got != cuotaInstancia(logico) {
		t.Fatalf("disco de siempre: tope %d; quería %d", got, cuotaInstancia(logico))
	}
	if err := os.Truncate(ruta, 3*logico); err != nil {
		t.Fatal(err)
	}
	if got := topeOverlayAlCrear(ruta, defaultOverlayMiB); got != cuotaInstancia(3*logico) {
		t.Fatalf("copia mayor que el disco: tope %d; quería %d", got, cuotaInstancia(3*logico))
	}
	if got := topeOverlayAlCrear(ruta, 8*defaultOverlayMiB); got != cuotaInstancia(8*logico) {
		t.Fatalf("disco pedido mayor: tope %d; quería %d", got, cuotaInstancia(8*logico))
	}
}

// El tope se apunta la primera vez y no se vuelve a medir: un invitado que
// alarga su overlay justo después de un thaw (StartedAt nuevo), antes del
// primer vistazo, ya no fija su propia línea base; ni tampoco después de que
// el daemon se reinicie.
func TestVigiaOverlaysNoReMideTrasThaw(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	antes := time.Now().Add(-time.Minute)
	for _, id := range []string{"thaw", "reinicio"} {
		mc := m.addForTest(id)
		m.mu.Lock()
		mc.StartedAt = &antes
		m.mu.Unlock()
		overlayDe(t, m, id, int64(defaultOverlayMiB)<<20)
	}
	if p := m.vueltaVigiaOverlays(); len(p) != 0 {
		t.Fatalf("primera vuelta paró %v; no quería ninguna", p)
	}

	// Thaw: ejecución nueva, y el fichero crece antes del primer vistazo.
	ahora := time.Now()
	m.mu.Lock()
	m.byID["thaw"].StartedAt = &ahora
	m.mu.Unlock()
	overlayDe(t, m, "thaw", 10<<30)
	if p := m.vueltaVigiaOverlays(); !slices.Equal(p, []string{"thaw"}) {
		t.Fatalf("tras el thaw paró %v; quería [thaw]", p)
	}
	if st := estadoVigia(m, "thaw"); st != api.StateStopped {
		t.Fatalf("thaw quedó %s; quería stopped", st)
	}

	// Reinicio del daemon: otro Manager con lo que quedó en state.json.
	m.Close()
	var guardada *api.Machine
	for _, mc := range readState(t, m) {
		if mc.ID == "reinicio" {
			guardada = mc.Clone()
		}
	}
	if guardada == nil || guardada.OverlayMaxBytes == 0 {
		t.Fatalf("el tope de reinicio no quedó en state.json: %+v", guardada)
	}
	m2 := newTestManager(t)
	m2.bus = events.New()
	m2.mu.Lock()
	m2.byID[guardada.ID] = guardada
	m2.mu.Unlock()
	overlayDe(t, m2, "reinicio", 10<<30)
	if p := m2.vueltaVigiaOverlays(); !slices.Equal(p, []string{"reinicio"}) {
		t.Fatalf("tras reiniciar el daemon paró %v; quería [reinicio]", p)
	}
}

// stopSi no para la máquina si ya no está en la ejecución que se midió.
//
// Las máquinas de estos tests no tienen VMM (PID 0): un PID inventado puede ser
// un proceso real del host, y Stop le mandaría la señal. La ejecución se
// distingue por StartedAt.
func TestStopSiNoParaOtraEjecucion(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	ahora := time.Now()
	mc := m.addForTest("m")
	m.mu.Lock()
	mc.StartedAt = &ahora
	m.mu.Unlock()
	antes := ahora.Add(-time.Minute)
	if _, err := m.stopSi("m", func(mc *api.Machine) bool { return mc.StartedAt.Equal(antes) }); !errors.Is(err, errOtraEjecucion) {
		t.Fatalf("stopSi de otra ejecución: %v; quería errOtraEjecucion", err)
	}
	if st := estadoVigia(m, "m"); st != api.StateRunning {
		t.Fatalf("quedó %s; quería running", st)
	}
	if _, err := m.stopSi("m", func(mc *api.Machine) bool { return mc.StartedAt.Equal(ahora) }); err != nil {
		t.Fatal(err)
	}
	if st := estadoVigia(m, "m"); st != api.StateStopped {
		t.Fatalf("quedó %s; quería stopped", st)
	}
}
