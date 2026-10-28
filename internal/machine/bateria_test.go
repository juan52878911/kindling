package machine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
)

// banco de pruebas del modo batería: un reloj, la CPU de cada invitado y lo
// que se pausó, reanudó y cambió de QoS, todo falso.
type bancoBateria struct {
	mu       sync.Mutex
	t        time.Time
	enBat    bool
	lecturas int
	cpu      map[string]int64 // por socket
	qos      map[int]bool
	pausadas []string
	reanud   []string
}

func nuevoBancoBateria(t *testing.T, m *Manager, cfg BatteryConfig) *bancoBateria {
	t.Helper()
	b := &bancoBateria{t: time.Unix(1_000_000, 0), enBat: true, cpu: map[string]int64{}, qos: map[int]bool{}}
	m.SetBattery(cfg)
	m.bat.ahora = func() time.Time { b.mu.Lock(); defer b.mu.Unlock(); return b.t }
	m.bat.enBateriaSO = func() (bool, error) { b.mu.Lock(); defer b.mu.Unlock(); b.lecturas++; return b.enBat, nil }
	m.bat.cpuDe = func(_ context.Context, sock string) (int64, int, error) {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.cpu[sock], 9000, nil
	}
	m.bat.qos = func(pid int, baja bool) error { b.mu.Lock(); defer b.mu.Unlock(); b.qos[pid] = baja; return nil }
	m.bat.pausar = func(_ context.Context, id string) error {
		b.mu.Lock()
		b.pausadas = append(b.pausadas, id)
		b.mu.Unlock()
		m.mu.Lock()
		m.byID[id].State = api.StatePaused
		m.mu.Unlock()
		return nil
	}
	m.bat.reanudar = func(_ context.Context, id string) error {
		b.mu.Lock()
		b.reanud = append(b.reanud, id)
		b.mu.Unlock()
		m.mu.Lock()
		m.byID[id].State = api.StateRunning
		m.mu.Unlock()
		return nil
	}
	return b
}

func (b *bancoBateria) pasar(d time.Duration) { b.mu.Lock(); b.t = b.t.Add(d); b.mu.Unlock() }

func (b *bancoBateria) gastar(sock string, ms int64) { b.mu.Lock(); b.cpu[sock] += ms; b.mu.Unlock() }

func maquinaBateria(m *Manager, id string, labels map[string]string) *api.Machine {
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.PID, mc.Labels = 100, labels
	m.socket[id] = "/s/" + id
	m.mu.Unlock()
	return mc
}

// En batería, una máquina ociosa baja de QoS a la primera vuelta que se la
// ve quieta y se pausa tras PauseAfter; una petición le devuelve la QoS, y
// volver al enchufe la reanuda.
func TestBateriaPausaLaOciosaYVuelveAlEnchufe(t *testing.T) {
	m := newTestManager(t)
	b := nuevoBancoBateria(t, m, BatteryConfig{Mode: config.BatteryAuto, PauseAfter: 60 * time.Second})
	maquinaBateria(m, "ociosa", nil)
	maquinaBateria(m, "ocupada", nil)
	maquinaBateria(m, "puertos", map[string]string{api.LabelPorts: "6379"})
	maquinaBateria(m, "servicio", map[string]string{api.LabelService: "x"})
	ctx := context.Background()

	for range 4 { // 4 vueltas de 30 s: 90 s ociosa tras la primera lectura
		m.vueltaBateria(ctx)
		b.gastar("/s/ocupada", 3000) // 10 % de un núcleo
		b.gastar("/s/ociosa", 10)
		b.pasar(30 * time.Second)
	}
	if len(b.pausadas) != 1 || b.pausadas[0] != "ociosa" {
		t.Fatalf("pausadas = %v, quería solo la ociosa", b.pausadas)
	}
	if b.qos[100] || b.qos[9000] {
		t.Fatalf("la QoS de la pausada sigue baja: %v", b.qos)
	}

	// Una petición que habla con el invitado la despierta.
	fin := m.Usar(ctx, "ociosa", true)
	fin()
	if len(b.reanud) != 1 {
		t.Fatalf("reanudadas = %v", b.reanud)
	}

	// Se vuelve a dormir y el Mac se enchufa: se reanuda sola.
	for range 4 {
		m.vueltaBateria(ctx)
		b.gastar("/s/ocupada", 3000)
		b.pasar(30 * time.Second)
	}
	if len(b.pausadas) != 2 {
		t.Fatalf("pausadas = %v, quería la ociosa otra vez", b.pausadas)
	}
	b.mu.Lock()
	b.enBat = false
	b.mu.Unlock()
	b.pasar(fuenteCache)
	m.vueltaBateria(ctx)
	if len(b.reanud) != 2 {
		t.Fatalf("al enchufar, reanudadas = %v", b.reanud)
	}
	if mc, _ := m.Get("ociosa"); mc.State != api.StateRunning {
		t.Fatalf("estado al enchufar = %s", mc.State)
	}
}

// Con una petición en curso (un shell, unos logs que se siguen) no se pausa
// por mucho que el invitado esté quieto; y la QoS baja se deshace al llegar
// la petición.
func TestBateriaNoPausaConPeticionEnCurso(t *testing.T) {
	m := newTestManager(t)
	b := nuevoBancoBateria(t, m, BatteryConfig{Mode: config.BatteryOn, PauseAfter: 90 * time.Second})
	maquinaBateria(m, "shell", nil)
	ctx := context.Background()
	m.vueltaBateria(ctx)
	b.pasar(30 * time.Second)
	m.vueltaBateria(ctx) // ociosa: QoS baja
	if !b.qos[100] || !b.qos[9000] {
		t.Fatalf("QoS tras una vuelta ociosa = %v", b.qos)
	}
	fin := m.Usar(ctx, "shell", false)
	if b.qos[100] || b.qos[9000] {
		t.Fatalf("una petición no devolvió la QoS: %v", b.qos)
	}
	for range 5 {
		b.pasar(30 * time.Second)
		m.vueltaBateria(ctx)
	}
	if len(b.pausadas) != 0 {
		t.Fatalf("pausó con una petición en curso: %v", b.pausadas)
	}
	fin()
	for range 3 {
		b.pasar(30 * time.Second)
		m.vueltaBateria(ctx)
	}
	if len(b.pausadas) != 1 {
		t.Fatalf("acabada la petición, pausadas = %v", b.pausadas)
	}
}

// Enchufado (o con el modo apagado) no se toca nada, y en auto la fuente de
// alimentación se pregunta como mucho una vez por fuenteCache.
func TestBateriaEnchufadoNoHaceNada(t *testing.T) {
	m := newTestManager(t)
	b := nuevoBancoBateria(t, m, BatteryConfig{Mode: config.BatteryAuto})
	b.enBat = false
	maquinaBateria(m, "a", nil)
	for range 10 {
		m.vueltaBateria(context.Background())
		b.pasar(10 * time.Second)
	}
	if len(b.pausadas) != 0 || len(b.qos) != 0 {
		t.Fatalf("enchufado: pausadas %v, qos %v", b.pausadas, b.qos)
	}
	if b.lecturas > 2 {
		t.Fatalf("%d lecturas de la fuente en 100 s", b.lecturas)
	}
	if got := m.intervaloVigilante(10 * time.Second); got != 10*time.Second {
		t.Fatalf("intervalo enchufado = %v", got)
	}

	m.SetBattery(BatteryConfig{Mode: config.BatteryOff})
	b.enBat = true
	m.vueltaBateria(context.Background())
	if len(b.qos) != 0 {
		t.Fatal("con battery_mode off tocó la QoS")
	}
	m.SetBattery(BatteryConfig{Mode: config.BatteryOn, PauseAfter: -1})
	if got := m.intervaloVigilante(10 * time.Second); got != 30*time.Second {
		t.Fatalf("intervalo en batería = %v", got)
	}
	for range 10 {
		m.vueltaBateria(context.Background())
		b.pasar(time.Hour)
	}
	if len(b.pausadas) != 0 {
		t.Fatalf("con PauseAfter -1 pausó: %v", b.pausadas)
	}
}

// El host durmió: el reloj de pared adelanta al monotónico.
func TestRelojHostVeElSueno(t *testing.T) {
	pared, mono := time.Unix(1_000_000, 0), time.Duration(0)
	r := relojHost{leer: func() (time.Time, time.Duration) { return pared, mono }}
	if r.durmio() != 0 {
		t.Fatal("la primera mirada no puede ver sueño")
	}
	// 10 s despierto, con medio segundo de deriva: nada.
	pared, mono = pared.Add(10*time.Second), mono+10*time.Second-500*time.Millisecond
	if d := r.durmio(); d != 0 {
		t.Fatalf("despierto, durmió %v", d)
	}
	// 1 h dormido y 1 s despierto: la pared avanza 1 h 1 s, el monotónico 1 s.
	pared, mono = pared.Add(time.Hour+time.Second), mono+time.Second
	if d := r.durmio(); d != time.Hour {
		t.Fatalf("durmió %v, quería 1h", d)
	}
	// Y la base se rearma: la siguiente mirada ya no lo cuenta.
	pared, mono = pared.Add(time.Second), mono+time.Second
	if d := r.durmio(); d != 0 {
		t.Fatalf("contó dos veces el sueño: %v", d)
	}
	// Con el reloj de verdad, recién mirado, no hay sueño.
	var real relojHost
	real.durmio()
	if d := real.durmio(); d != 0 {
		t.Fatalf("reloj de verdad: durmió %v", d)
	}
}

func TestFuenteEsBateria(t *testing.T) {
	if !fuenteEsBateria("Now drawing from 'Battery Power'\n -InternalBattery-0 (id=1)\t80%; discharging") {
		t.Fatal("no vio la batería")
	}
	if fuenteEsBateria("Now drawing from 'AC Power'\n -InternalBattery-0 (id=1)\t100%; charged") {
		t.Fatal("vio batería enchufado")
	}
}
