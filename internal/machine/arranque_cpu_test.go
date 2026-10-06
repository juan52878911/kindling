package machine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// cgroupFalso registra, en orden, cada techo que el código escribe en cpu.max,
// por cgroup. Es el "fake cgroup writer" de A2: lo que permite afirmar la
// secuencia impulso → CPUPct sin cgroups de verdad ni root.
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
// "contesta" cuando la prueba cierra el canal devuelto. El host tiene 8
// núcleos y KLING_READY_BOOST va sin poner, salvo que la prueba diga otra cosa.
func managerConCgroupFalso(t *testing.T) (*Manager, *cgroupFalso, chan struct{}) {
	t.Helper()
	t.Setenv("KLING_READY_BOOST", "")
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
		nucleos: 8,
	}
	// GET /ready del agente: lo que diga fijarListo; antes, nadie contesta.
	m.pruebasListo = func(_ context.Context, id string) (api.GuestReady, error) {
		v, ok := listosFalsos.Load(id)
		if !ok {
			return api.GuestReady{}, errListoConexion
		}
		return v.(api.GuestReady), nil
	}
	return m, cg, contesta
}

// listosFalsos es la respuesta de GET /ready de cada máquina de prueba (los
// ids no se repiten entre pruebas).
var listosFalsos sync.Map

// addConIP da de alta una máquina que el host alcanza (WaitReady no pregunta
// a una sin IP).
func addConIP(m *Manager, id string) *api.Machine {
	mc := m.addForTest(id)
	listosFalsos.Delete(id) // lo de una pasada anterior (-count)
	m.mu.Lock()
	mc.IP = "10.0.0.2"
	m.mu.Unlock()
	return mc
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

// fijarListo hace que el agente de id conteste a /ready con el estado dado.
func fijarListo(_ *Manager, id, estado string) {
	var st api.GuestReady
	switch estado {
	case api.ReadyUnknown: // sin sonda
		st = api.GuestReady{Ready: true}
	case api.ReadyWaiting:
		st = api.GuestReady{Probe: true, Detail: "not yet"}
	case api.ReadyYes:
		st = api.GuestReady{Probe: true, Ready: true}
	case api.ReadyFailed:
		st = api.GuestReady{HasHooks: true, Hooks: api.HooksFailed, Detail: "boom"}
	}
	listosFalsos.Store(id, st)
}

// impulsoQueSeVe es el CPUBoostPct que devuelve Get (lo que ven ps e inspect).
func impulsoQueSeVe(t *testing.T, m *Manager, id string) int {
	t.Helper()
	mc, ok := m.Get(id)
	if !ok {
		t.Fatalf("no está %s", id)
	}
	return mc.CPUBoostPct
}

// arranqueComoRun reproduce la forma en que Run usa el impulso: sube el techo
// al meter el proceso en su cgroup, difiere fin(), y en el camino de éxito lo
// entrega a quien espera el fin del arranque. err != nil es un fallo tras
// subirlo (en Run, el de las carpetas vivas).
func arranqueComoRun(m *Manager, id string, pct, vcpus int, fijo bool, err error) error {
	impulso := m.nuevoImpulso(id, pct, vcpus, fijo)
	defer impulso.fin()
	if warn := m.limitCPU(id, os.Getpid(), impulso.tope); warn != "" {
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

func TestTopeImpulso(t *testing.T) {
	for _, c := range []struct{ pct, vcpus, nucleos, want int }{
		{50, 1, 8, 100},  // una vCPU: un núcleo
		{50, 0, 8, 100},  // sin vCPU anotadas, como una
		{50, 4, 8, 400},  // todas sus vCPU
		{50, 4, 2, 200},  // nunca más que el host
		{50, 4, 0, 100},  // host desconocido: un núcleo
		{600, 4, 8, 600}, // ya tiene más: se queda con el suyo
		{400, 4, 8, 400}, // == configurado: no hay impulso
	} {
		if got := topeImpulso(c.pct, c.vcpus, c.nucleos); got != c.want {
			t.Errorf("topeImpulso(%d, %d, %d) = %d, quería %d", c.pct, c.vcpus, c.nucleos, got, c.want)
		}
	}
}

func TestPoliticaImpulso(t *testing.T) {
	for _, c := range []struct {
		env   string
		listo bool
		plazo time.Duration
	}{
		{"", true, plazoImpulsoListo},
		{"0", false, 0},
		{"off", false, 0},
		{"FALSE", false, 0},
		{"90s", true, 90 * time.Second},
		{"2m", true, 2 * time.Minute},
		{"basura", true, plazoImpulsoListo},
		{"-5s", true, plazoImpulsoListo},
	} {
		t.Setenv("KLING_READY_BOOST", c.env)
		listo, plazo := politicaImpulso()
		if listo != c.listo || plazo != c.plazo {
			t.Errorf("KLING_READY_BOOST=%q: (%v, %s), quería (%v, %s)", c.env, listo, plazo, c.listo, c.plazo)
		}
	}
}

// Imagen sin sonda: el impulso dura hasta que contesta su agente (la vigía
// anota "unknown" en la primera respuesta), y en ese momento baja.
func TestImpulsoSinSondaBajaCuandoContestaElAgente(t *testing.T) {
	m, cg, contesta := managerConCgroupFalso(t)
	id := "a2c00001000000000"
	addConIP(m, id)

	if err := arranqueComoRun(m, id, 50, 2, false, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * pasoImpulsoCPU)
	if got := cg.de(id); !reflect.DeepEqual(got, []int{200}) {
		t.Fatalf("antes de que conteste el agente cpu.max = %v, quería [200]", got)
	}
	if got := impulsoQueSeVe(t, m, id); got != 200 {
		t.Fatalf("Get().CPUBoostPct = %d, quería 200", got)
	}

	fijarListo(m, id, api.ReadyUnknown)
	close(contesta)
	esperarSecuencia(t, cg, id, []int{200, 50})
	if got := impulsoQueSeVe(t, m, id); got != 0 {
		t.Fatalf("tras bajar Get().CPUBoostPct = %d, quería 0", got)
	}
}

// Imagen con sonda: contestar el agente no basta; el impulso sigue mientras
// la sonda dice "waiting" y baja cuando pasa. Y se publica el motivo.
func TestImpulsoConSondaBajaAlPasarLaSonda(t *testing.T) {
	m, cg, contesta := managerConCgroupFalso(t)
	m.bus = events.New()
	evs, baja := m.bus.Subscribe()
	defer baja()
	id := "a2c00002000000000"
	addConIP(m, id)

	if err := arranqueComoRun(m, id, 50, 4, false, nil); err != nil {
		t.Fatal(err)
	}
	fijarListo(m, id, api.ReadyWaiting)
	close(contesta)
	time.Sleep(3 * 100 * time.Millisecond)
	if got := cg.de(id); !reflect.DeepEqual(got, []int{400}) {
		t.Fatalf("con la sonda en waiting cpu.max = %v, quería [400]", got)
	}

	fijarListo(m, id, api.ReadyYes)
	esperarSecuencia(t, cg, id, []int{400, 50})
	select {
	case ev := <-evs:
		if ev.Type != api.EvBoostEnded || ev.ID != id || !strings.Contains(ev.Message, "ready probe passed") ||
			!strings.Contains(ev.Message, "400% -> 50%") {
			t.Fatalf("evento = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no se publicó el fin del impulso")
	}
}

// Una sonda que no pasa nunca no deja la máquina impulsada: al agotar el
// plazo (KLING_READY_BOOST), baja igual.
func TestImpulsoBajaAlAgotarElPlazoDeLaSonda(t *testing.T) {
	m, cg, contesta := managerConCgroupFalso(t)
	t.Setenv("KLING_READY_BOOST", "300ms")
	id := "a2c00003000000000"
	addConIP(m, id)

	if err := arranqueComoRun(m, id, 50, 1, false, nil); err != nil {
		t.Fatal(err)
	}
	fijarListo(m, id, api.ReadyWaiting)
	close(contesta)
	esperarSecuencia(t, cg, id, []int{100, 50})
}

// Un agente que nunca contesta (imagen sin agente) no deja la máquina con el
// impulso para siempre: al agotar plazoImpulsoCPU, el techo baja igual.
func TestImpulsoBajaAlAgotarElPlazoSinAgente(t *testing.T) {
	m, cg, _ := managerConCgroupFalso(t)
	m.pruebasCPU.plazo = 50 * time.Millisecond
	id := "a2c00004000000000"
	addConIP(m, id)

	if err := arranqueComoRun(m, id, 30, 1, false, nil); err != nil {
		t.Fatal(err)
	}
	esperarSecuencia(t, cg, id, []int{100, 30})
}

// KLING_READY_BOOST=0: el impulso de antes. Un núcleo aunque tenga más vCPU,
// y baja en cuanto contesta el agente aunque la sonda siga en waiting.
func TestImpulsoDesactivadoEsElDeAntes(t *testing.T) {
	m, cg, contesta := managerConCgroupFalso(t)
	t.Setenv("KLING_READY_BOOST", "0")
	id := "a2c00005000000000"
	addConIP(m, id)

	if err := arranqueComoRun(m, id, 50, 4, false, nil); err != nil {
		t.Fatal(err)
	}
	fijarListo(m, id, api.ReadyWaiting)
	close(contesta)
	esperarSecuencia(t, cg, id, []int{100, 50})
}

// Un -cpu-pct explícito manda también al arrancar: una sola escritura, sin
// impulso, sin goroutine y sin nada que enseñar en ps.
func TestImpulsoNoPisaUnCPUPctExplicito(t *testing.T) {
	m, cg, contesta := managerConCgroupFalso(t)
	id := "a2c00006000000000"
	addConIP(m, id)

	if err := arranqueComoRun(m, id, 30, 4, true, nil); err != nil {
		t.Fatal(err)
	}
	if got := impulsoQueSeVe(t, m, id); got != 0 {
		t.Fatalf("Get().CPUBoostPct = %d con -cpu-pct explícito", got)
	}
	close(contesta)
	time.Sleep(5 * pasoImpulsoCPU)
	if got := cg.de(id); !reflect.DeepEqual(got, []int{30}) {
		t.Fatalf("cpu.max = %v, quería [30]", got)
	}
}

// El camino de error: un fallo después de subir el techo lo baja en el
// defer, en el acto, sin goroutine ni espera al agente.
func TestImpulsoCaminoDeErrorBajaElTope(t *testing.T) {
	m, cg, _ := managerConCgroupFalso(t)
	id := "a2c00007000000000"
	addConIP(m, id)

	if err := arranqueComoRun(m, id, 50, 1, false, errors.New("boom")); err == nil {
		t.Fatal("quería el error del arranque")
	}
	if got := cg.de(id); !reflect.DeepEqual(got, []int{100, 50}) {
		t.Fatalf("tras un arranque fallido cpu.max = %v, quería [100 50]", got)
	}
	if got := impulsoQueSeVe(t, m, id); got != 0 {
		t.Fatalf("tras un arranque fallido Get().CPUBoostPct = %d", got)
	}
}

// Un pánico entre subir y entregar también pasa por el defer.
func TestImpulsoPanicoBajaElTope(t *testing.T) {
	m, cg, _ := managerConCgroupFalso(t)
	id := "a2c00008000000000"
	addConIP(m, id)

	func() {
		defer func() { _ = recover() }()
		impulso := m.nuevoImpulso(id, 50, 1, false)
		defer impulso.fin()
		m.limitCPU(id, os.Getpid(), impulso.tope)
		panic("boom")
	}()
	if got := cg.de(id); !reflect.DeepEqual(got, []int{100, 50}) {
		t.Fatalf("tras un pánico cpu.max = %v, quería [100 50]", got)
	}
}

// Si la máquina deja de correr mientras se espera a la sonda (un freeze, un
// fallo del VMM), el techo baja ya: no se sigue esperando algo que no está.
func TestImpulsoBajaSiLaMaquinaSeCongelaEsperandoLaSonda(t *testing.T) {
	m, cg, contesta := managerConCgroupFalso(t)
	id := "a2c00009000000000"
	mc := addConIP(m, id)

	if err := arranqueComoRun(m, id, 50, 2, false, nil); err != nil {
		t.Fatal(err)
	}
	fijarListo(m, id, api.ReadyWaiting)
	close(contesta)
	time.Sleep(2 * 100 * time.Millisecond)
	m.mu.Lock()
	mc.State = api.StateWarm
	m.mu.Unlock()
	esperarSecuencia(t, cg, id, []int{200, 50})
}

// Igual si se para mientras aún se espera al agente.
func TestImpulsoBajaSiLaMaquinaDejaDeCorrer(t *testing.T) {
	m, cg, _ := managerConCgroupFalso(t)
	id := "a2c0000a000000000"
	mc := addConIP(m, id)

	if err := arranqueComoRun(m, id, 50, 1, false, nil); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	mc.State = api.StateFailed
	m.mu.Unlock()
	esperarSecuencia(t, cg, id, []int{100, 50})
}

// Un rm durante el impulso: la máquina sale del mapa y releaseCPU borra su
// cgroup. La goroutine termina, no recrea nada y el impulso deja de verse.
func TestImpulsoSeLimpiaSiSeBorraLaMaquina(t *testing.T) {
	m, cg, contesta := managerConCgroupFalso(t)
	id := "a2c0000b000000000"
	addConIP(m, id)

	if err := arranqueComoRun(m, id, 50, 2, false, nil); err != nil {
		t.Fatal(err)
	}
	fijarListo(m, id, api.ReadyWaiting)
	close(contesta)
	// Primero el cgroup (killMachine → releaseCPU) y después la entrada, para
	// que la goroutine no llegue a escribir en un cgroup que aún existe.
	if err := os.RemoveAll(m.dirCgroup(id)); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	delete(m.byID, id)
	m.mu.Unlock()

	limite := time.Now().Add(5 * time.Second)
	for m.impulsoVigente(id, api.StateRunning) != 0 {
		if time.Now().After(limite) {
			t.Fatal("el impulso de una máquina borrada sigue vigente")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(m.dirCgroup(id)); !os.IsNotExist(err) {
		t.Fatalf("bajar el techo recreó el cgroup: %v", err)
	}
	if got := cg.de(id); !reflect.DeepEqual(got, []int{200}) {
		t.Fatalf("cpu.max = %v, quería [200] (sin escritura tras borrarlo)", got)
	}
}

// Cerrar el Manager (parar el daemon) baja el techo de lo que arrancaba: el
// VMM sobrevive al daemon y la goroutine que iba a bajarlo, no.
func TestImpulsoBajaAlCerrarElManager(t *testing.T) {
	m, cg, _ := managerConCgroupFalso(t)
	id := "a2c0000c000000000"
	addConIP(m, id)

	if err := arranqueComoRun(m, id, 50, 1, false, nil); err != nil {
		t.Fatal(err)
	}
	m.Close()
	esperarSecuencia(t, cg, id, []int{100, 50})
}

// Un arranque nuevo de la misma máquina (freeze → thaw) jubila el impulso del
// anterior: la goroutine tardía de aquel no le acorta el suyo.
func TestImpulsoNuevoJubilaAlAnterior(t *testing.T) {
	m, cg, _ := managerConCgroupFalso(t)
	id := "a2c0000d000000000"
	addConIP(m, id)
	if err := os.MkdirAll(m.dirCgroup(id), 0o755); err != nil {
		t.Fatal(err)
	}

	viejo := m.nuevoImpulso(id, 50, 2, false)
	nuevo := m.nuevoImpulso(id, 50, 2, false)
	viejo.bajar("machine is no longer running")
	if got := cg.de(id); len(got) != 0 {
		t.Fatalf("el impulso jubilado escribió cpu.max = %v", got)
	}
	if got := m.impulsoVigente(id, api.StateRunning); got != 200 {
		t.Fatalf("impulso vigente = %d, quería 200", got)
	}
	nuevo.bajar("ready probe passed")
	if got := cg.de(id); !reflect.DeepEqual(got, []int{50}) {
		t.Fatalf("cpu.max = %v, quería [50]", got)
	}
}

// La forma de Thaw y runFrom: el VMM nace en su cgroup con el techo de
// arranque; si la memoria ya trae el "listo" (lo normal), baja en el acto, sin
// goroutine; el defer ya no escribe nada más.
func TestImpulsoComoThawYaListo(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroupParaLanzar solo prepara el cgroup en Linux")
	}
	m, cg, _ := managerConCgroupFalso(t)
	id := "a2c0000e000000000"
	addConIP(m, id)

	func() {
		impulso := m.nuevoImpulso(id, 50, 2, false)
		defer impulso.fin()
		f := m.cgroupParaLanzar(id, impulso.tope)
		if f == nil {
			t.Fatal("cgroupParaLanzar no preparó el cgroup")
		}
		defer f.Close()
		if got := cg.de(id); !reflect.DeepEqual(got, []int{200}) {
			t.Fatalf("al nacer cpu.max = %v, quería [200]", got)
		}
		// Lo que contestó al resync: listo, sin ganchos.
		impulso.entregarRestaurada(&api.GuestReady{Probe: true, Ready: true})
	}()
	if got := cg.de(id); !reflect.DeepEqual(got, []int{200, 50}) {
		t.Fatalf("tras el thaw cpu.max = %v, quería [200 50]", got)
	}
}

// Thaw de algo que aún no estaba listo (ganchos en marcha): el impulso sigue
// tras devolver la máquina y baja cuando pasa la sonda.
func TestImpulsoComoThawEsperandoLosGanchos(t *testing.T) {
	m, cg, _ := managerConCgroupFalso(t)
	id := "a2c0000f000000000"
	addConIP(m, id)

	func() {
		impulso := m.nuevoImpulso(id, 50, 1, false)
		defer impulso.fin()
		m.limitCPU(id, os.Getpid(), impulso.tope)
		fijarListo(m, id, api.ReadyWaiting)
		impulso.entregarRestaurada(&api.GuestReady{Probe: true, Ready: true, HasHooks: true})
	}()
	time.Sleep(2 * 100 * time.Millisecond)
	if got := cg.de(id); !reflect.DeepEqual(got, []int{100}) {
		t.Fatalf("con ganchos en marcha cpu.max = %v, quería [100]", got)
	}
	fijarListo(m, id, api.ReadyYes)
	esperarSecuencia(t, cg, id, []int{100, 50})
}

// El fallo que vio el laboratorio: el agente escucha, pero su /ready aún no
// ha contestado nada (Machine.Ready vacío, que también es "sin sonda"). Eso
// no es fin del arranque: el impulso sigue hasta que /ready lo dice.
func TestImpulsoAgenteSinRespuestaDeListoNoBaja(t *testing.T) {
	m, cg, contesta := managerConCgroupFalso(t)
	id := "a2c00014000000000"
	addConIP(m, id)

	if err := arranqueComoRun(m, id, 50, 2, false, nil); err != nil {
		t.Fatal(err)
	}
	close(contesta)
	time.Sleep(300 * time.Millisecond)
	if got := cg.de(id); !reflect.DeepEqual(got, []int{200}) {
		t.Fatalf("sin respuesta de /ready cpu.max = %v, quería [200]", got)
	}
	fijarListo(m, id, api.ReadyYes)
	esperarSecuencia(t, cg, id, []int{200, 50})
}

// Unos ganchos que fallan terminan el arranque: el impulso baja.
func TestImpulsoBajaSiFallanLosGanchos(t *testing.T) {
	m, cg, contesta := managerConCgroupFalso(t)
	id := "a2c00015000000000"
	addConIP(m, id)

	if err := arranqueComoRun(m, id, 50, 1, false, nil); err != nil {
		t.Fatal(err)
	}
	fijarListo(m, id, api.ReadyFailed)
	close(contesta)
	esperarSecuencia(t, cg, id, []int{100, 50})
}

// Restaurada sin agente (o con uno anterior a /ready): no hay sonda que
// esperar, baja en el acto.
func TestImpulsoRestauradaSinAgenteBajaYa(t *testing.T) {
	m, cg, _ := managerConCgroupFalso(t)
	id := "a2c00016000000000"
	addConIP(m, id)

	impulso := m.nuevoImpulso(id, 50, 2, false)
	m.limitCPU(id, os.Getpid(), impulso.tope)
	impulso.entregarRestaurada(nil)
	impulso.fin()
	if got := cg.de(id); !reflect.DeepEqual(got, []int{200, 50}) {
		t.Fatalf("cpu.max = %v, quería [200 50]", got)
	}
}

// Thaw o runFrom que fallan tras nacer en el cgroup (LoadSnapshot con el TSC
// invalidado, por ejemplo): el defer baja el techo igual.
func TestImpulsoComoThawFallido(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroupParaLanzar solo prepara el cgroup en Linux")
	}
	m, cg, _ := managerConCgroupFalso(t)
	id := "a2c00010000000000"

	thaw := func() error {
		impulso := m.nuevoImpulso(id, 20, 1, false)
		defer impulso.fin()
		if f := m.cgroupParaLanzar(id, impulso.tope); f != nil {
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

// Con un techo configurado que ya cubre todas sus vCPU no hay impulso que
// deshacer: una sola escritura, y sin goroutine.
func TestImpulsoConTopeQueYaCubreLasVCPUNoEscribeDosVeces(t *testing.T) {
	m, cg, _ := managerConCgroupFalso(t)
	id := "a2c00011000000000"
	addConIP(m, id)

	if err := arranqueComoRun(m, id, 200, 2, false, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * pasoImpulsoCPU)
	if got := cg.de(id); !reflect.DeepEqual(got, []int{200}) {
		t.Fatalf("cpu.max = %v, quería [200]", got)
	}
}

// Sin cgroups (sin delegación, o macOS) no se escribe nada ni se lanza nada.
func TestImpulsoSinCgroupsNoHaceNada(t *testing.T) {
	m, cg, _ := managerConCgroupFalso(t)
	m.cgroupRoot = ""
	id := "a2c00012000000000"
	addConIP(m, id)

	if err := arranqueComoRun(m, id, 50, 2, false, nil); err != nil {
		t.Fatal(err)
	}
	if got := cg.de(id); len(got) != 0 {
		t.Fatalf("sin cgroups se escribió cpu.max = %v", got)
	}
	if got := impulsoQueSeVe(t, m, id); got != 0 {
		t.Fatalf("sin cgroups Get().CPUBoostPct = %d", got)
	}
}

// Bajar el techo de un cgroup que releaseCPU ya borró (la máquina se paró
// antes de terminar de arrancar) no es un error ni lo recrea.
func TestImpulsoSobreCgroupBorradoNoLoRecrea(t *testing.T) {
	m := newTestManager(t)
	m.cgroupRoot = t.TempDir()
	id := "a2c00013000000000"
	impulso := m.nuevoImpulso(id, 50, 1, false)
	impulso.bajar("machine is no longer running")
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
	viva, muerta := "a2c0000c000000001", "a2c0000d000000001"
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
