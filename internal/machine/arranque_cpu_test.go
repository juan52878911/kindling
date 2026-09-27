package machine

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// cgroupFalso registra, en orden, cada techo que el código escribe en cpu.max,
// por cgroup. Es el "fake cgroup writer" de A2: lo que permite afirmar la
// secuencia 100 → CPUPct sin cgroups de verdad ni root.
type cgroupFalso struct {
	mu       sync.Mutex
	escritos map[string][]int // nombre del cgroup (kl-xxxxxxxx) -> techos
	cambio   chan struct{}
}

func (c *cgroupFalso) escribir(dir string, pct int) error {
	if _, err := os.Stat(dir); err != nil {
		return err // como el cgroupfs: sin directorio no hay cpu.max
	}
	c.mu.Lock()
	c.escritos[filepath.Base(dir)] = append(c.escritos[filepath.Base(dir)], pct)
	c.mu.Unlock()
	select {
	case c.cambio <- struct{}{}:
	default:
	}
	return nil
}

func (c *cgroupFalso) de(id string) []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int(nil), c.escritos["kl-"+id[:8]]...)
}

// managerConCgroupFalso es un Manager de prueba con un árbol de cgroups en un
// temporal y la escritura de cpu.max registrada por cgroupFalso. El agente
// "contesta" cuando la prueba cierra el canal devuelto.
func managerConCgroupFalso(t *testing.T) (*Manager, *cgroupFalso, chan struct{}) {
	t.Helper()
	m := newTestManager(t)
	m.cgroupRoot = t.TempDir()
	cg := &cgroupFalso{escritos: map[string][]int{}, cambio: make(chan struct{}, 1)}
	contesta := make(chan struct{})
	m.pruebasCPU = &ganchosCPU{
		escribir: cg.escribir,
		agente: func(string) bool {
			select {
			case <-contesta:
				return true
			default:
				return false
			}
		},
	}
	return m, cg, contesta
}

// esperarSecuencia espera (sin sleeps fijos) a que el cgroup de id tenga la
// secuencia want.
func esperarSecuencia(t *testing.T, cg *cgroupFalso, id string, want []int) {
	t.Helper()
	limite := time.After(5 * time.Second)
	for !reflect.DeepEqual(cg.de(id), want) {
		select {
		case <-cg.cambio:
		case <-time.After(10 * time.Millisecond):
		case <-limite:
			t.Fatalf("cpu.max de %s = %v, quería %v", id, cg.de(id), want)
		}
	}
}

// arranqueComoRun reproduce la forma en que Run usa el impulso: sube el techo
// al meter el proceso en su cgroup, difiere fin(), y en el camino de éxito lo
// entrega a quien espera al agente. err != nil es un fallo tras subirlo (en
// Run, el de las carpetas vivas).
func arranqueComoRun(m *Manager, id string, pct int, err error) error {
	impulso := m.nuevoImpulso(id, pct)
	defer impulso.fin()
	if warn := m.limitCPU(id, os.Getpid(), topeArranque(pct)); warn != "" {
		return errors.New(warn)
	}
	if err != nil {
		return err
	}
	impulso.entregar()
	return nil
}

func TestTopeArranque(t *testing.T) {
	for _, c := range []struct{ pct, want int }{
		{50, 100}, {1, 100}, {100, 100}, {150, 150}, {400, 400},
	} {
		if got := topeArranque(c.pct); got != c.want {
			t.Errorf("topeArranque(%d) = %d, quería %d", c.pct, got, c.want)
		}
	}
}

// El caso normal de Run: un núcleo entero mientras el agente no contesta, y
// el techo configurado en cuanto lo hace. Ni antes ni nunca.
func TestImpulsoBajaCuandoContestaElAgente(t *testing.T) {
	m, cg, contesta := managerConCgroupFalso(t)
	id := "a2c00001000000000"
	m.addForTest(id)

	if err := arranqueComoRun(m, id, 50, nil); err != nil {
		t.Fatal(err)
	}
	// Run ya volvió y el agente aún no contesta: sigue el techo de arranque.
	time.Sleep(5 * pasoImpulsoCPU)
	if got := cg.de(id); !reflect.DeepEqual(got, []int{100}) {
		t.Fatalf("antes de que conteste el agente cpu.max = %v, quería [100]", got)
	}

	close(contesta)
	esperarSecuencia(t, cg, id, []int{100, 50})
}

// Un agente que nunca contesta (imagen sin agente) no deja la máquina con un
// núcleo entero para siempre: al agotar el plazo, el techo baja igual.
func TestImpulsoBajaAlAgotarElPlazoSinAgente(t *testing.T) {
	m, cg, _ := managerConCgroupFalso(t)
	m.pruebasCPU.plazo = 50 * time.Millisecond
	id := "a2c00002000000000"
	m.addForTest(id)

	if err := arranqueComoRun(m, id, 30, nil); err != nil {
		t.Fatal(err)
	}
	esperarSecuencia(t, cg, id, []int{100, 30})
}

// El camino de error: un fallo después de subir el techo lo baja en el
// defer, en el acto, sin goroutine ni espera al agente.
func TestImpulsoCaminoDeErrorBajaElTope(t *testing.T) {
	m, cg, _ := managerConCgroupFalso(t)
	id := "a2c00003000000000"
	m.addForTest(id)

	if err := arranqueComoRun(m, id, 50, errors.New("boom")); err == nil {
		t.Fatal("quería el error del arranque")
	}
	if got := cg.de(id); !reflect.DeepEqual(got, []int{100, 50}) {
		t.Fatalf("tras un arranque fallido cpu.max = %v, quería [100 50]", got)
	}
}

// Un pánico entre subir y entregar también pasa por el defer.
func TestImpulsoPanicoBajaElTope(t *testing.T) {
	m, cg, _ := managerConCgroupFalso(t)
	id := "a2c00004000000000"
	m.addForTest(id)

	func() {
		defer func() { _ = recover() }()
		impulso := m.nuevoImpulso(id, 50)
		defer impulso.fin()
		m.limitCPU(id, os.Getpid(), topeArranque(50))
		panic("boom")
	}()
	if got := cg.de(id); !reflect.DeepEqual(got, []int{100, 50}) {
		t.Fatalf("tras un pánico cpu.max = %v, quería [100 50]", got)
	}
}

// Si la máquina deja de correr mientras se espera al agente (un rm, un
// freeze), el techo baja ya: no se sigue sondeando algo que no está.
func TestImpulsoBajaSiLaMaquinaDejaDeCorrer(t *testing.T) {
	m, cg, _ := managerConCgroupFalso(t)
	id := "a2c00005000000000"
	mc := m.addForTest(id)

	if err := arranqueComoRun(m, id, 50, nil); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	mc.State = api.StateWarm
	m.mu.Unlock()
	esperarSecuencia(t, cg, id, []int{100, 50})
}

// Cerrar el Manager (parar el daemon) baja el techo de lo que arrancaba: el
// VMM sobrevive al daemon y la goroutine que iba a bajarlo, no.
func TestImpulsoBajaAlCerrarElManager(t *testing.T) {
	m, cg, _ := managerConCgroupFalso(t)
	id := "a2c00006000000000"
	m.addForTest(id)

	if err := arranqueComoRun(m, id, 50, nil); err != nil {
		t.Fatal(err)
	}
	m.Close()
	esperarSecuencia(t, cg, id, []int{100, 50})
}

// La forma de Thaw y runFrom: el VMM nace en su cgroup con el techo de
// arranque, el agente contesta (resync) y se baja; el defer ya no escribe
// nada más, porque bajar es idempotente.
func TestImpulsoComoThaw(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroupParaLanzar solo prepara el cgroup en Linux")
	}
	m, cg, _ := managerConCgroupFalso(t)
	id := "a2c00007000000000"

	func() {
		impulso := m.nuevoImpulso(id, 50)
		defer impulso.fin()
		f := m.cgroupParaLanzar(id, topeArranque(50))
		if f == nil {
			t.Fatal("cgroupParaLanzar no preparó el cgroup")
		}
		defer f.Close()
		if got := cg.de(id); !reflect.DeepEqual(got, []int{100}) {
			t.Fatalf("al nacer cpu.max = %v, quería [100]", got)
		}
		impulso.bajar() // tras el resync
		impulso.bajar()
	}()
	if got := cg.de(id); !reflect.DeepEqual(got, []int{100, 50}) {
		t.Fatalf("tras el thaw cpu.max = %v, quería [100 50]", got)
	}
}

// Thaw o runFrom que fallan tras nacer en el cgroup (LoadSnapshot con el TSC
// invalidado, por ejemplo): el defer baja el techo igual.
func TestImpulsoComoThawFallido(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroupParaLanzar solo prepara el cgroup en Linux")
	}
	m, cg, _ := managerConCgroupFalso(t)
	id := "a2c00008000000000"

	thaw := func() error {
		impulso := m.nuevoImpulso(id, 20)
		defer impulso.fin()
		if f := m.cgroupParaLanzar(id, topeArranque(20)); f != nil {
			defer f.Close()
		}
		return errors.New("loading snapshot: TSC")
	}
	if err := thaw(); err == nil {
		t.Fatal("quería el error")
	}
	if got := cg.de(id); !reflect.DeepEqual(got, []int{100, 20}) {
		t.Fatalf("tras un thaw fallido cpu.max = %v, quería [100 20]", got)
	}
}

// Con un techo configurado de un núcleo o más no hay impulso que deshacer:
// una sola escritura, y sin goroutine.
func TestImpulsoConTopeDeUnNucleoOMasNoEscribeDosVeces(t *testing.T) {
	m, cg, _ := managerConCgroupFalso(t)
	id := "a2c00009000000000"
	m.addForTest(id)

	if err := arranqueComoRun(m, id, 150, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * pasoImpulsoCPU)
	if got := cg.de(id); !reflect.DeepEqual(got, []int{150}) {
		t.Fatalf("cpu.max = %v, quería [150]", got)
	}
}

// Sin cgroups (sin delegación, o macOS) no se escribe nada ni se lanza nada.
func TestImpulsoSinCgroupsNoHaceNada(t *testing.T) {
	m, cg, _ := managerConCgroupFalso(t)
	m.cgroupRoot = ""
	id := "a2c0000a000000000"
	m.addForTest(id)

	if err := arranqueComoRun(m, id, 50, nil); err != nil {
		t.Fatal(err)
	}
	if got := cg.de(id); len(got) != 0 {
		t.Fatalf("sin cgroups se escribió cpu.max = %v", got)
	}
}

// Bajar el techo de un cgroup que releaseCPU ya borró (la máquina se paró
// antes de que contestara su agente) no es un error ni lo recrea.
func TestImpulsoSobreCgroupBorradoNoLoRecrea(t *testing.T) {
	m := newTestManager(t)
	m.cgroupRoot = t.TempDir()
	id := "a2c0000b000000000"
	impulso := m.nuevoImpulso(id, 50)
	impulso.bajar()
	if _, err := os.Stat(m.dirCgroup(id)); !os.IsNotExist(err) {
		t.Fatalf("bajar el techo recreó el cgroup: %v", err)
	}
}

// La escritura real, sin gancho: el formato de cpu.max.
func TestEscribirCPUMaxFormato(t *testing.T) {
	m := newTestManager(t)
	dir := t.TempDir()
	if err := m.escribirCPUMax(dir, 50); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "cpu.max"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b)); got != "50000 100000" {
		t.Fatalf("cpu.max = %q, quería \"50000 100000\"", got)
	}
}

// Si el daemon muere con un arranque en marcha, su goroutine muere con él y el
// cgroup se queda con el techo de arranque. Al volver, reaplicarTopesCPU lo
// devuelve al configurado en las máquinas vivas, y no toca las demás.
func TestReaplicarTopesCPUTrasReiniciar(t *testing.T) {
	m, cg, _ := managerConCgroupFalso(t)
	viva, muerta := "a2c0000c000000000", "a2c0000d000000000"
	for _, id := range []string{viva, muerta} {
		if err := os.MkdirAll(m.dirCgroup(id), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mv := m.addForTest(viva)
	mm := m.addForTest(muerta)
	m.mu.Lock()
	mv.PID, mv.CPUPct = 1234, 40
	mm.PID, mm.CPUPct, mm.State = 0, 40, api.StateWarm
	m.mu.Unlock()

	m.reaplicarTopesCPU()
	if got := cg.de(viva); !reflect.DeepEqual(got, []int{40}) {
		t.Errorf("máquina viva: cpu.max = %v, quería [40]", got)
	}
	if got := cg.de(muerta); len(got) != 0 {
		t.Errorf("máquina sin proceso: cpu.max = %v, quería nada", got)
	}
}
