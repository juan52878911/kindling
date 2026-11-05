package machine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
)

// bancoPresion: el nivel del host, el reloj, la CPU y la huella de cada VMM, y
// lo que se apretó y congeló, todo falso.
type bancoPresion struct {
	mu         sync.Mutex
	t          time.Time
	nivel      int
	cpu        map[int]time.Duration // por PID
	huella     map[int]int64
	apretadas  []string
	congeladas []string
	m          *Manager
}

func nuevoBancoPresion(t *testing.T, m *Manager, cfg MemPressureConfig) *bancoPresion {
	t.Helper()
	b := &bancoPresion{t: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC), nivel: NivelPresionNormal,
		cpu: map[int]time.Duration{}, huella: map[int]int64{}, m: m}
	m.SetMemPressure(cfg)
	p := &m.pres
	p.ahora = b.now
	m.bat.ahora = b.now
	p.fuente = func() (int, error) { b.mu.Lock(); defer b.mu.Unlock(); return b.nivel, nil }
	p.cpuDe = func(pid int, _ string) (time.Duration, bool) {
		b.mu.Lock()
		defer b.mu.Unlock()
		c, ok := b.cpu[pid]
		return c, ok
	}
	p.huellaDe = func(pid int, _ string) int64 { b.mu.Lock(); defer b.mu.Unlock(); return b.huella[pid] }
	p.apretar = func(_ context.Context, id string) (int, error) {
		b.mu.Lock()
		b.apretadas = append(b.apretadas, id)
		b.mu.Unlock()
		m.mu.Lock()
		if m.squeezedAt == nil {
			m.squeezedAt = map[string]time.Time{}
		}
		m.squeezedAt[id] = b.now()
		m.mu.Unlock()
		return 100, nil
	}
	p.congelar = func(_ context.Context, id string, sigue func(*api.Machine) bool) error {
		mc, ok := m.Get(id)
		if !ok || !sigue(mc) {
			return errYaNoToca
		}
		b.mu.Lock()
		b.congeladas = append(b.congeladas, id)
		b.mu.Unlock()
		return nil
	}
	return b
}

func (b *bancoPresion) now() time.Time                  { b.mu.Lock(); defer b.mu.Unlock(); return b.t }
func (b *bancoPresion) pasar(d time.Duration)           { b.mu.Lock(); b.t = b.t.Add(d); b.mu.Unlock() }
func (b *bancoPresion) poner(nivel int)                 { b.mu.Lock(); b.nivel = nivel; b.mu.Unlock() }
func (b *bancoPresion) gastar(pid int, d time.Duration) { b.mu.Lock(); b.cpu[pid] += d; b.mu.Unlock() }

// vuelta es una vuelta del vigilante con lo que decida hecho en el acto.
func (b *bancoPresion) vuelta() planPresion {
	plan := b.m.vueltaPresion()
	if !plan.vacio() {
		b.m.aplicarPresion(context.Background(), plan)
	}
	return plan
}

func (b *bancoPresion) hechas() (apretadas, congeladas []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.apretadas...), append([]string(nil), b.congeladas...)
}

// maquinaPresion da de alta una máquina que corre desde hace una hora, con
// PID pid, huella mib y la CPU a 0.
func maquinaPresion(b *bancoPresion, id string, pid int, mib int64, labels map[string]string) *api.Machine {
	m := b.m
	mc := m.addForTest(id)
	desde := b.now().Add(-time.Hour)
	m.mu.Lock()
	mc.PID, mc.Labels, mc.StartedAt = pid, labels, &desde
	m.socket[id] = "/s/" + id
	m.mu.Unlock()
	b.mu.Lock()
	b.cpu[pid] = 0
	b.huella[pid] = mib
	b.mu.Unlock()
	return mc
}

func ids(cs []candidatoPresion) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}

func igual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// El plan: sin presión nada; en aviso, apretar las dos de más huella (a
// igualdad, la que lleva más sin uso), saltándose las que comparten memoria
// con un dorado y las apretadas hace poco; en crítico, congelar la mayor y
// apretar las siguientes.
func TestPlanificarPresion(t *testing.T) {
	ahora := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	hace := func(d time.Duration) time.Time { return ahora.Add(-d) }
	cands := []candidatoPresion{
		{ID: "chica", HuellaMiB: 100, SinUsoDesde: hace(time.Hour)},
		{ID: "grande", HuellaMiB: 900, SinUsoDesde: hace(5 * time.Minute)},
		{ID: "media-vieja", HuellaMiB: 400, SinUsoDesde: hace(time.Hour)},
		{ID: "media-nueva", HuellaMiB: 400, SinUsoDesde: hace(3 * time.Minute)},
		{ID: "compartida", HuellaMiB: 2000, SinUsoDesde: hace(time.Hour), Compartida: true},
	}
	if p := planificarPresion(NivelPresionNormal, cands, nil, ahora); !p.vacio() {
		t.Fatalf("sin presión: %+v", p)
	}
	p := planificarPresion(NivelPresionAviso, cands, nil, ahora)
	if !igual(ids(p.apretar), []string{"grande", "media-vieja"}) || len(p.congelar) != 0 {
		t.Fatalf("aviso: apretar %v congelar %v", ids(p.apretar), ids(p.congelar))
	}
	// La grande se apretó hace un minuto: no se repite.
	p = planificarPresion(NivelPresionAviso, cands, map[string]time.Time{"grande": hace(time.Minute)}, ahora)
	if !igual(ids(p.apretar), []string{"media-vieja", "media-nueva"}) {
		t.Fatalf("aviso con la grande recién apretada: %v", ids(p.apretar))
	}
	// Pasado presionReapretar, sí.
	p = planificarPresion(NivelPresionAviso, cands, map[string]time.Time{"grande": hace(presionReapretar)}, ahora)
	if !igual(ids(p.apretar), []string{"grande", "media-vieja"}) {
		t.Fatalf("aviso pasado el freno: %v", ids(p.apretar))
	}
	// Crítico: la compartida es la de más huella y se congela (congelarla sí
	// devuelve memoria); se aprietan las dos siguientes.
	p = planificarPresion(NivelPresionCritico, cands, nil, ahora)
	if !igual(ids(p.congelar), []string{"compartida"}) || !igual(ids(p.apretar), []string{"grande", "media-vieja"}) {
		t.Fatalf("crítico: congelar %v apretar %v", ids(p.congelar), ids(p.apretar))
	}
}

// La histéresis: subir es inmediato; bajar, solo tras presionBajada seguidos
// por debajo, y un repunte reinicia la cuenta.
func TestHisteresisPresion(t *testing.T) {
	var p presionMemoria
	t0 := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	if !p.actualizar(1, t0) || p.nivel != NivelPresionNormal {
		t.Fatalf("primera lectura: %d", p.nivel)
	}
	if !p.actualizar(4, t0.Add(time.Second)) || p.nivel != NivelPresionCritico {
		t.Fatalf("subir: %d", p.nivel)
	}
	if p.actualizar(2, t0.Add(10*time.Second)) || p.nivel != NivelPresionCritico {
		t.Fatalf("bajar al instante: %d", p.nivel)
	}
	if p.actualizar(4, t0.Add(20*time.Second)) {
		t.Fatal("el repunte cambió el nivel")
	}
	if p.actualizar(2, t0.Add(30*time.Second)) || p.actualizar(2, t0.Add(30*time.Second+presionBajada-time.Second)) {
		t.Fatal("bajó antes de presionBajada desde el repunte")
	}
	if !p.actualizar(1, t0.Add(30*time.Second+presionBajada)) || p.nivel != NivelPresionNormal {
		t.Fatalf("bajar tras presionBajada: %d", p.nivel)
	}
	// Un valor raro de la fuente se lleva al nivel más cercano por abajo.
	if !p.actualizar(3, t0.Add(time.Hour)) || p.nivel != NivelPresionAviso {
		t.Fatalf("3: %d", p.nivel)
	}
}

// De punta a punta sobre el Manager: en aviso solo se aprieta la ociosa (ni la
// que gasta CPU, ni la que tiene un exec en curso, ni la de puertos, ni la
// usada hace poco), con freno entre vueltas; en crítico se congela; y con
// daemon.memory_pressure=off no se hace nada.
func TestPresionAprietaYCongelaSoloOciosas(t *testing.T) {
	m := newTestManager(t)
	b := nuevoBancoPresion(t, m, MemPressureConfig{Mode: config.MemoryPressureAuto})
	maquinaPresion(b, "ociosa", 11, 500, nil)
	maquinaPresion(b, "ociosa-chica", 12, 200, nil)
	maquinaPresion(b, "cpu", 13, 900, nil)
	maquinaPresion(b, "exec", 14, 900, nil)
	maquinaPresion(b, "puertos", 15, 900, map[string]string{api.LabelPorts: "8080"})
	maquinaPresion(b, "reciente", 16, 900, nil)
	ctx := context.Background()

	// La de exec tiene una petición en curso; la reciente acaba de terminar
	// una.
	fin := m.Usar(ctx, "exec", false)
	defer fin()
	m.bat.mu.Lock()
	m.bat.usoDe("reciente").ultimo = b.now()
	m.bat.mu.Unlock()

	// Sin presión no se toca nada (ni se lee la CPU).
	b.vuelta()
	b.pasar(10 * time.Second)
	if a, c := b.hechas(); len(a)+len(c) != 0 {
		t.Fatalf("sin presión: %v %v", a, c)
	}

	// Aviso: la primera vuelta solo toma la referencia de CPU.
	b.poner(NivelPresionAviso)
	if p := b.vuelta(); !p.vacio() {
		t.Fatalf("primera vuelta con aviso: %+v", p)
	}
	b.pasar(10 * time.Second)
	b.gastar(13, time.Second) // 10 % de un núcleo
	b.gastar(11, 10*time.Millisecond)
	b.vuelta()
	if a, _ := b.hechas(); !igual(a, []string{"ociosa", "ociosa-chica"}) {
		t.Fatalf("aviso: apretadas %v", a)
	}
	// Siguen en aviso: dentro del enfriamiento no se actúa, y después las ya
	// apretadas no se repiten.
	for range 6 {
		b.pasar(10 * time.Second)
		b.gastar(13, time.Second)
		b.vuelta()
	}
	if a, c := b.hechas(); len(a) != 2 || len(c) != 0 {
		t.Fatalf("aviso sostenido: apretadas %v congeladas %v", a, c)
	}
	if got := m.Telemetria(); got.PresionNivel != NivelPresionAviso || got.PresionApretones < 2 {
		t.Fatalf("telemetría: %+v", got)
	}

	// Crítico: se congela la ociosa de más huella; la de exec, nunca.
	b.poner(NivelPresionCritico)
	b.pasar(10 * time.Second)
	b.gastar(13, time.Second)
	b.vuelta()
	if _, c := b.hechas(); !igual(c, []string{"ociosa"}) {
		t.Fatalf("crítico: congeladas %v", c)
	}

	// Apagado: nada, aunque siga en crítico.
	m.SetMemPressure(MemPressureConfig{Mode: config.MemoryPressureOff})
	b.pasar(time.Minute)
	if p := b.vuelta(); !p.vacio() {
		t.Fatalf("apagado: %+v", p)
	}
	if got := m.Telemetria(); got.PresionNivel != 0 {
		t.Fatalf("apagado: nivel %d", got.PresionNivel)
	}
}

// Una petición que llega entre la foto y la congelación la salva: el freeze
// revalida con el cerrojo tomado (sigue) y no la toca.
func TestPresionNoCongelaSiLlegaUnaPeticion(t *testing.T) {
	m := newTestManager(t)
	b := nuevoBancoPresion(t, m, MemPressureConfig{})
	maquinaPresion(b, "ociosa", 11, 500, nil)
	b.poner(NivelPresionCritico)
	b.vuelta() // referencia de CPU
	b.pasar(10 * time.Second)
	plan := m.vueltaPresion()
	if !igual(ids(plan.congelar), []string{"ociosa"}) {
		t.Fatalf("plan: %+v", plan)
	}
	fin := m.Usar(context.Background(), "ociosa", false)
	m.aplicarPresion(context.Background(), plan)
	fin()
	if _, c := b.hechas(); len(c) != 0 {
		t.Fatalf("congeló una máquina con una petición en curso: %v", c)
	}
}

// Un Manager al que nadie le llamó SetMemPressure (los de las pruebas) no lee
// el nivel del host en el que corre.
func TestPresionSinConfigurarNoHaceNada(t *testing.T) {
	m := newTestManager(t)
	leida := false
	m.pres.fuente = func() (int, error) { leida = true; return NivelPresionCritico, nil }
	if p := m.vueltaPresion(); !p.vacio() || leida {
		t.Fatalf("sin configurar: plan %+v, leída %v", p, leida)
	}
}
