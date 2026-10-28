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
	// fallarSoltar: cuántos Freeze(false) seguidos fallan desde ya.
	fallarSoltar int
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
	if m.fallarSoltar > 0 {
		m.fallarSoltar--
		return errors.New("SIGCONT: operation not permitted")
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

// La CPU del auxiliar llega en escalones (proc_pid_rusage apunta el tiempo de
// un vCPU al salir del invitado: hasta 10 ms, medido en un M4). El cubo cobra
// después lo que no vio: la cuota media es la misma y ninguna parada pasa de
// cpuPausaMax.
func TestCuboConMedidaEnEscalones(t *testing.T) {
	const (
		periodo = 20 * time.Millisecond
		escalon = 10 * time.Millisecond
	)
	for _, tc := range []struct{ pct, vcpus, carga int }{{25, 1, 1}, {50, 1, 1}, {10, 1, 1}, {50, 2, 2}} {
		t0 := time.Unix(0, 0)
		c := cubo{fichas: cpuRafaga * time.Duration(tc.pct) / 100, t: t0}
		ahora, cpu := t0, time.Duration(0)
		var maxParada time.Duration
		for ahora.Sub(t0) < 30*time.Second {
			c.cuenta(ahora, cpu/escalon*escalon, tc.pct) // lo visto: hasta el último escalón
			parar, dormir := c.decide(tc.pct, tc.vcpus, periodo)
			if parar > 0 {
				ahora = ahora.Add(parar)
				maxParada = max(maxParada, parar)
				continue
			}
			ahora = ahora.Add(dormir)
			cpu += dormir * time.Duration(tc.carga)
		}
		frac := 100 * float64(cpu) / float64(ahora.Sub(t0))
		if want := float64(tc.pct); frac < want*0.95 || frac > want*1.05 {
			t.Errorf("pct %d, carga %d, medida en escalones de 10 ms: gasta %.1f %%, quería %.0f %%", tc.pct, tc.carga, frac, want)
		}
		if maxParada > cpuPausaMax {
			t.Errorf("pct %d: parada de %v", tc.pct, maxParada)
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

// Hasta que el agente escucha, el techo es max(pct, 100 × min(vCPU, núcleos
// del host)), como el impulso de arranque de Linux: 2 vCPU con pct 50 corren
// con las dos vCPU a tope, no a medio núcleo.
func TestReguladorImpulsoDeArranque(t *testing.T) {
	m := nuevoSimulado(2)
	r := rigSimulado(t, 2, m, false, nil)
	r.must("PUT", "/kling/cpu", `{"pct":50}`)
	// Con el impulso (200 con 2 vCPU) no hay techo efectivo: ni para ni mide.
	_, _, _, n0 := m.foto()
	time.Sleep(200 * time.Millisecond)
	if _, _, paradas, n1 := m.foto(); len(paradas) != 0 || n1 != n0 {
		t.Fatalf("arrancando: %d paradas y %d medidas, quería ninguna", len(paradas), n1-n0)
	}
	r.srv.marcarListo()
	// Se deja pasar un segundo: la ráfaga que el cubo acumuló durante el
	// impulso se gasta ahí y no infla la medida del régimen (con el host
	// cargado salía 53 %).
	t0, _, _, _ := m.foto()
	m.hasta(t, t0.Add(time.Second))
	t0, c0, _, _ := m.foto()
	m.hasta(t, t0.Add(5*time.Second))
	t1, c1, _, _ := m.foto()
	if frac := 100 * float64(c1-c0) / float64(t1.Sub(t0)); frac < 47 || frac > 53 {
		t.Fatalf("listo gasta %.1f %%, quería 50", frac)
	}
}

// Sin techo efectivo (pct >= 100 × vCPU, o 0) no se para nunca, y tampoco se
// mide ni se duerme a plazo: el regulador espera a que algo cambie, sin
// despertar al proceso (ni el reloj falso avanza). Al poner un techo de
// verdad, empieza a regular sin esperar a nada más.
func TestReguladorSinTechoEfectivo(t *testing.T) {
	m := nuevoSimulado(2)
	r := rigSimulado(t, 2, m, true, nil)
	for _, pct := range []string{`{"pct":200}`, `{"pct":0}`} {
		r.must("PUT", "/kling/cpu", pct)
		t0, _, _, n0 := m.foto()
		time.Sleep(200 * time.Millisecond)
		t1, _, paradas, n1 := m.foto()
		if len(paradas) != 0 || n1 != n0 || !t1.Equal(t0) {
			t.Fatalf("%s: %d paradas, %d medidas y %v de reloj sin techo efectivo", pct, len(paradas), n1-n0, t1.Sub(t0))
		}
	}
	r.must("PUT", "/kling/cpu", `{"pct":50}`)
	t0, _, _, _ := m.foto()
	m.hasta(t, t0.Add(2*time.Second))
	if _, _, paradas, _ := m.foto(); len(paradas) == 0 {
		t.Fatal("con techo 50 no regula")
	}
}

// Sin techo no se pregunta al agente: el impulso de arranque solo sirve con
// techo.
func TestSinTechoNoSeEsperaAlAgente(t *testing.T) {
	m := nuevoSimulado(1)
	r := rigSimulado(t, 1, m, false, nil)
	r.must("PUT", "/kling/cpu", `{"pct":0}`)
	r.srv.mu.Lock()
	esperando := r.srv.cpuEsperando
	r.srv.mu.Unlock()
	if esperando {
		t.Fatal("sin techo se lanzó esperarAgente")
	}
}

// Con techo y la VM ociosa, el sueño entre medidas crece hasta cpuReposoMax:
// pocas medidas por segundo, no las 10-20 de un cubo lleno.
func TestReguladorOciosoDuermeLargo(t *testing.T) {
	m := nuevoSimulado(0)
	r := rigSimulado(t, 1, m, true, nil)
	r.must("PUT", "/kling/cpu", `{"pct":50}`)
	t0, _, _, n0 := m.foto()
	m.hasta(t, t0.Add(10*time.Second))
	t1, _, paradas, n1 := m.foto()
	if len(paradas) != 0 {
		t.Fatalf("paró %d veces una VM ociosa", len(paradas))
	}
	if por := float64(n1-n0) / t1.Sub(t0).Seconds(); por > 3 {
		t.Fatalf("%.1f medidas por segundo con la VM ociosa, quería <= 3", por)
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
	// En pausa el regulador no tiene nada que hacer: espera sin medir (ni
	// avanza el reloj falso), así que se deja pasar tiempo de verdad.
	time.Sleep(200 * time.Millisecond)
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

// GET /kling/cpu da la CPU gastada y el pid del auxiliar: el modo batería del
// daemon mira con eso si la VM está ociosa y a quién bajarle la QoS.
func TestGetKlingCPUDaCPUyAuxiliar(t *testing.T) {
	m := nuevoSimulado(1)
	r := rigSimulado(t, 1, m, true, nil)
	r.srv.d.HelperPID = func() int { return 4242 }
	m.dormir(1500 * time.Millisecond)
	var out struct {
		CPUMS     int64 `json:"cpu_ms"`
		HelperPID int   `json:"helper_pid"`
	}
	if err := json.Unmarshal([]byte(r.must("GET", "/kling/cpu", "")), &out); err != nil {
		t.Fatal(err)
	}
	if out.CPUMS != 1500 || out.HelperPID != 4242 {
		t.Fatalf("GET /kling/cpu = %+v", out)
	}
}
