package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// simulado es un auxiliar de Apple de mentira sobre un reloj falso: gasta
// `carga` núcleos mientras no esté parado, y el tiempo solo avanza cuando el
// regulador duerme. Hace de reloj (reloj), de CPUTime y de Freeze a la vez.
type simulado struct {
	mu      sync.Mutex
	t       time.Time
	carga   int
	parado  bool
	desde   time.Time // desde cuándo está parado
	cpu     time.Duration
	paradas []time.Duration
	medidas int
	fallar  bool // Freeze(true) falla
}

func nuevoSimulado(carga int) *simulado {
	return &simulado{t: time.Unix(1000, 0), carga: carga}
}

func (m *simulado) ahora() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.t
}

func (m *simulado) dormir(d time.Duration) {
	m.mu.Lock()
	if !m.parado {
		m.cpu += d * time.Duration(m.carga)
	}
	m.t = m.t.Add(d)
	m.mu.Unlock()
	runtime.Gosched()
}

func (m *simulado) CPUTime() (time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.medidas++
	return m.cpu, nil
}

func (m *simulado) Freeze(stop bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if stop {
		if m.fallar {
			return errors.New("operation not permitted")
		}
		m.parado, m.desde = true, m.t
		return nil
	}
	if m.parado {
		m.paradas = append(m.paradas, m.t.Sub(m.desde))
	}
	m.parado = false
	return nil
}

// foto es el estado del simulado en un momento.
func (m *simulado) foto() (t time.Time, cpu time.Duration, paradas []time.Duration, medidas int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.t, m.cpu, append([]time.Duration(nil), m.paradas...), m.medidas
}

// hasta espera (de verdad) a que el reloj falso pase de t.
func (m *simulado) hasta(tb testing.TB, t time.Time) {
	tb.Helper()
	limite := time.Now().Add(10 * time.Second)
	for m.ahora().Before(t) {
		if time.Now().After(limite) {
			tb.Fatalf("el reloj falso no llegó a %v (va por %v)", t, m.ahora())
		}
		time.Sleep(time.Millisecond)
	}
}

// rigSimulado arranca una VM de mentira con vcpus vCPU cuyo regulador corre
// sobre el simulado. listo: el agente ya contestó (sin impulso de arranque).
func rigSimulado(t *testing.T, vcpus int, m *simulado, listo bool, logf func(string, ...any)) *rig {
	t.Helper()
	r := newRig(t)
	if logf != nil {
		r.srv.d.Logf = logf
	}
	r.configure(t.TempDir())
	r.must("PUT", "/machine-config", fmt.Sprintf(`{"vcpu_count":%d,"mem_size_mib":512}`, vcpus))
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	r.srv.d.CPUTime, r.srv.d.Freeze = m.CPUTime, m.Freeze
	r.srv.cpuReloj = m
	r.srv.cpuListo = listo
	if !listo {
		r.srv.cpuGracia = time.Hour // que no llegue a estar listo
	}
	t.Cleanup(r.srv.Shutdown)
	return r
}

// El cubo, sin goroutinas: una VM que gasta k núcleos bajo un techo pct acaba
// gastando pct, y cada parada dura un periodo (más la deuda de la última
// medida), no las ventanas enteras de antes.
func TestCuboCumpleElTecho(t *testing.T) {
	for _, tc := range []struct{ pct, vcpus, carga int }{
		{50, 1, 1}, {50, 2, 2}, {50, 2, 1}, {100, 2, 2}, {25, 4, 4}, {150, 2, 2},
	} {
		const periodo = 20 * time.Millisecond
		t0 := time.Unix(0, 0)
		c := cubo{fichas: cpuRafaga * time.Duration(tc.pct) / 100, t: t0}
		ahora, cpu := t0, time.Duration(0)
		var maxParada time.Duration
		paradas := 0
		for ahora.Sub(t0) < 20*time.Second {
			c.cuenta(ahora, cpu, tc.pct)
			parar, dormir := c.decide(tc.pct, tc.vcpus, periodo)
			if parar > 0 {
				ahora = ahora.Add(parar)
				maxParada = max(maxParada, parar)
				paradas++
				continue
			}
			ahora = ahora.Add(dormir)
			cpu += dormir * time.Duration(tc.carga)
		}
		got := 100 * float64(cpu) / float64(ahora.Sub(t0))
		want := float64(min(tc.pct, 100*tc.carga))
		if got < want*0.97 || got > want*1.03 {
			t.Errorf("pct %d, %d vCPU, carga %d: gasta %.1f %%, quería %.0f %%", tc.pct, tc.vcpus, tc.carga, got, want)
		}
		// La parada es un periodo más la deuda de la última medida, que como
		// mucho es lo que se gasta entre dos medidas seguidas.
		if tope := periodo + cpuMedidaMin*time.Duration(tc.carga*100/tc.pct) + time.Millisecond; maxParada > tope {
			t.Errorf("pct %d, %d vCPU, carga %d: una parada de %v con periodo %v", tc.pct, tc.vcpus, tc.carga, maxParada, periodo)
		}
		if tc.pct >= 100*tc.carga && paradas > 0 {
			t.Errorf("pct %d con carga %d no debía parar (%d paradas)", tc.pct, tc.carga, paradas)
		}
	}
}

// Una VM ociosa llena el cubo y se mide poco: como mucho una vez por
// ráfaga/vCPU, y sin paradas.
func TestCuboOciosoMidePoco(t *testing.T) {
	t0 := time.Unix(0, 0)
	c := cubo{fichas: cpuRafaga / 2, t: t0}
	ahora, medidas := t0, 0
	for ahora.Sub(t0) < 10*time.Second {
		c.cuenta(ahora, 0, 50)
		parar, dormir := c.decide(50, 1, 20*time.Millisecond)
		if parar > 0 {
			t.Fatal("paró una VM ociosa")
		}
		ahora = ahora.Add(dormir)
		medidas++
	}
	if medidas > 10*1000/50+1 { // cubo lleno: 50 ms de fichas a 1 vCPU
		t.Fatalf("%d medidas en 10 s de VM ociosa", medidas)
	}
}

// Una deuda enorme (un salto del contador, p. ej.) no deja la VM parada más de
// cpuPausaMax seguidos.
func TestCuboDeudaAcotada(t *testing.T) {
	c := cubo{t: time.Unix(0, 0)}
	c.cuenta(c.t.Add(time.Millisecond), time.Hour, 50)
	parar, _ := c.decide(50, 1, 20*time.Millisecond)
	if parar != cpuPausaMax {
		t.Fatalf("parada = %v, quería %v", parar, cpuPausaMax)
	}
}

// La goroutine entera con el reloj y la CPU inyectados: 2 vCPU a tope con
// techo 50 gastan medio núcleo, parando periodos cortos con el freno de
// señales, sin tocar la pausa del framework.
func TestReguladorConRelojFalso(t *testing.T) {
	m := nuevoSimulado(2)
	r := rigSimulado(t, 2, m, true, nil)
	vm, _, _ := r.f.last()
	r.must("PUT", "/kling/cpu", `{"pct":50}`)
	t0, c0, _, _ := m.foto()
	m.hasta(t, t0.Add(10*time.Second))
	t1, c1, paradas, _ := m.foto()
	frac := 100 * float64(c1-c0) / float64(t1.Sub(t0))
	if frac < 48 || frac > 52 {
		t.Fatalf("gasta %.1f %% de un núcleo, quería 50", frac)
	}
	var maxParada time.Duration
	for _, p := range paradas {
		maxParada = max(maxParada, p)
	}
	if len(paradas) < 100 || maxParada > 30*time.Millisecond {
		t.Fatalf("%d paradas, la mayor de %v: quería muchas y de ~20 ms", len(paradas), maxParada)
	}
	for _, op := range vm.ops() {
		if op == "pause" || op == "resume" {
			t.Fatalf("con freno de señales no se pausa por el framework: %v", vm.ops())
		}
	}
	var out struct {
		ThrottledMS int64 `json:"throttled_ms"`
	}
	if err := json.Unmarshal([]byte(r.must("GET", "/kling/cpu", "")), &out); err != nil {
		t.Fatal(err)
	}
	if out.ThrottledMS < 7000 { // 2 núcleos a 50 %: parada 3/4 del tiempo
		t.Fatalf("throttled_ms = %d tras 10 s, quería ~7500", out.ThrottledMS)
	}
}

// Hasta que el agente escucha, el techo es max(pct, 100), como el impulso de
// arranque de Linux: 2 vCPU con pct 50 corren a un núcleo, no a medio ni a dos.
func TestReguladorImpulsoDeArranque(t *testing.T) {
	m := nuevoSimulado(2)
	r := rigSimulado(t, 2, m, false, nil)
	r.must("PUT", "/kling/cpu", `{"pct":50}`)
	t0, c0, _, _ := m.foto()
	m.hasta(t, t0.Add(5*time.Second))
	t1, c1, _, _ := m.foto()
	if frac := 100 * float64(c1-c0) / float64(t1.Sub(t0)); frac < 97 || frac > 103 {
		t.Fatalf("arrancando gasta %.1f %%, quería 100", frac)
	}
	r.srv.mu.Lock()
	r.srv.cpuListo = true
	r.srv.mu.Unlock()
	t0, c0, _, _ = m.foto()
	m.hasta(t, t0.Add(5*time.Second))
	t1, c1, _, _ = m.foto()
	if frac := 100 * float64(c1-c0) / float64(t1.Sub(t0)); frac < 47 || frac > 53 {
		t.Fatalf("listo gasta %.1f %%, quería 50", frac)
	}
}

// Sin techo efectivo (pct >= 100 × vCPU) no se para nunca.
func TestReguladorSinTechoEfectivo(t *testing.T) {
	m := nuevoSimulado(2)
	r := rigSimulado(t, 2, m, true, nil)
	r.must("PUT", "/kling/cpu", `{"pct":200}`)
	t0, _, _, _ := m.foto()
	m.hasta(t, t0.Add(2*time.Second))
	if _, _, paradas, _ := m.foto(); len(paradas) != 0 {
		t.Fatalf("%d paradas con pct 200 y 2 vCPU", len(paradas))
	}
}

// Si el núcleo pausa la VM mientras el regulador tiene parado el auxiliar,
// primero se reanuda el auxiliar (si no, el framework no contestaría) y luego
// se pausa la VM, que se queda en pausa: el regulador no la toca.
func TestPausaDelNucleoConElAuxiliarParado(t *testing.T) {
	m := nuevoSimulado(1)
	r := rigSimulado(t, 1, m, true, nil)
	vm, _, _ := r.f.last()
	r.must("PUT", "/kling/cpu", `{"pct":10}`)
	limite := time.Now().Add(5 * time.Second)
	for {
		r.srv.frenoMu.Lock()
		cong := r.srv.congelado
		r.srv.frenoMu.Unlock()
		if cong {
			break
		}
		if time.Now().After(limite) {
			t.Fatal("el regulador no llegó a parar el auxiliar")
		}
		time.Sleep(50 * time.Microsecond)
	}
	r.must("PATCH", "/vm", `{"state":"Paused"}`)
	m.mu.Lock()
	parado := m.parado
	m.mu.Unlock()
	if parado {
		t.Fatal("se pausó la VM con el auxiliar aún parado")
	}
	t0, _, _, _ := m.foto()
	m.hasta(t, t0.Add(time.Second))
	if out := r.must("GET", "/", ""); !strings.Contains(out, `"Paused"`) {
		t.Fatalf("estado tras pausar = %s", out)
	}
	vm.mu.Lock()
	enMarcha := vm.enMarcha
	vm.mu.Unlock()
	if enMarcha {
		t.Fatal("la VM que pausó el núcleo está corriendo")
	}
	m.mu.Lock()
	parado = m.parado
	m.mu.Unlock()
	if parado {
		t.Fatal("el regulador paró el auxiliar de una VM en pausa")
	}
}

// Si las señales no se pueden mandar, se avisa una vez y se pausa la VM por
// el framework: el techo se cumple igual.
func TestFrenoQueFallaPausaLaVM(t *testing.T) {
	m := nuevoSimulado(1)
	m.fallar = true
	var avisos []string
	var mu sync.Mutex
	r := rigSimulado(t, 1, m, true, func(f string, a ...any) {
		mu.Lock()
		avisos = append(avisos, f)
		mu.Unlock()
	})
	vm, _, _ := r.f.last()
	r.must("PUT", "/kling/cpu", `{"pct":50}`)
	limite := time.Now().Add(5 * time.Second)
	for {
		pausas := 0
		for _, op := range vm.ops() {
			if op == "pause" {
				pausas++
			}
		}
		if pausas >= 3 {
			break
		}
		if time.Now().After(limite) {
			t.Fatalf("no pausó por el framework: %v", vm.ops())
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, a := range avisos {
		if strings.Contains(a, "could not stop the VM's helper") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d avisos del freno, quería 1: %q", n, avisos)
	}
}
