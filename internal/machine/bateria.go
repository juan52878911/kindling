package machine

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
)

// EL MODO BATERÍA (macOS).
//
// Con el Mac enchufado nada cambia. Con batería (daemon.battery_mode: auto,
// lo normal; "on" lo fuerza, "off" lo apaga) el daemon:
//
//   - baja la QoS (PRIO_DARWIN_BG: núcleos de eficiencia, E/S en segundo
//     plano) del kling-vz y del auxiliar de Apple de cada máquina ociosa, y se
//     la devuelve en cuanto le llega una petición;
//   - pausa (no congela) la que lleva BatteryConfig.PauseAfter ociosa: en
//     pausa el auxiliar no gasta CPU ni despierta al Mac, y despertarla es
//     reanudar, sin restaurar nada. Volver al enchufe reanuda las que pausó;
//   - espacia las vueltas del vigilante (vigilanteBateria).
//
// Ociosa es: ninguna petición en curso contra ella (exec, shell, ficheros,
// llamadas al invitado, logs que se siguen: ver Usar) ni terminada hace menos
// de PauseAfter, y su invitado gastando menos de cpuOciosaPct de un núcleo
// entre dos vueltas. Nunca se pausa una que alguien puede estar usando sin
// pasar por el daemon: las que publican puertos (kling.ports), las del
// planificador y los fondos (service, pool), las de un grafo, las de kling db
// y las que tienen carpetas vivas.

// BatteryConfig es la configuración del modo batería.
type BatteryConfig struct {
	Mode       string        // config.BatteryAuto (o ""), BatteryOn, BatteryOff
	PauseAfter time.Duration // < 0: no pausar nunca
}

const (
	// pausaBateriaDef es BatteryConfig.PauseAfter por defecto.
	pausaBateriaDef = 60 * time.Second
	// vigilanteBateria multiplica el intervalo del vigilante en batería.
	vigilanteBateria = 3
	// cpuOciosaPct es lo que puede gastar un invitado ocioso, en % de un
	// núcleo: un redis sin clientes gasta ~0,5 % y un agente solo, ~0,1 %.
	cpuOciosaPct = 2
	// fuenteCache es cuánto vale la última lectura de la fuente de
	// alimentación (preguntar cuesta un proceso: ver enBateriaAhora).
	fuenteCache = time.Minute
)

// usoMaquina es lo que el modo batería sabe de una máquina.
type usoMaquina struct {
	enCurso int       // peticiones en curso (Usar)
	ultimo  time.Time // cuándo acabó la última

	cpuMS int64     // CPU del invitado en la última vuelta (-1: no se sabe)
	cpuT  time.Time // cuándo se leyó
	// ociosaDesde: desde cuándo se la ve ociosa; cero = no lo está.
	ociosaDesde time.Time

	qosBaja bool  // se le bajó la QoS
	pids    []int // a quién (kling-vz y auxiliar)
	pausada bool  // la pausó el modo batería (y no otro)
}

// bateria es el estado del modo batería del Manager.
type bateria struct {
	mu  sync.Mutex
	cfg BatteryConfig
	uso map[string]*usoMaquina

	// La fuente de alimentación, cacheada.
	enBat  bool
	leidaT time.Time
	antes  bool // en batería en la vuelta anterior

	// ahora, enBateriaSO, cpuDe, qos, pausar y reanudar son puntos de
	// inyección de las pruebas; nil = los de verdad.
	ahora       func() time.Time
	enBateriaSO func() (bool, error)
	cpuDe       func(ctx context.Context, sock string) (ms int64, aux int, err error)
	qos         func(pid int, baja bool) error
	pausar      func(ctx context.Context, id string) error
	reanudar    func(ctx context.Context, id string) error
}

func (m *Manager) pausarBat(ctx context.Context, id string) error {
	if m.bat.pausar != nil {
		return m.bat.pausar(ctx, id)
	}
	_, err := m.Pause(ctx, id)
	return err
}

func (m *Manager) reanudarBat(ctx context.Context, id string) error {
	if m.bat.reanudar != nil {
		return m.bat.reanudar(ctx, id)
	}
	_, err := m.Thaw(ctx, id)
	return err
}

// SetBattery fija la configuración del modo batería. Se llama al arrancar el
// daemon.
func (m *Manager) SetBattery(c BatteryConfig) {
	if c.PauseAfter == 0 {
		c.PauseAfter = pausaBateriaDef
	}
	m.bat.mu.Lock()
	m.bat.cfg = c
	m.bat.leidaT = time.Time{}
	m.bat.mu.Unlock()
}

func (b *bateria) now() time.Time {
	if b.ahora != nil {
		return b.ahora()
	}
	return time.Now()
}

// usoDe da (y crea) el uso de id. Con b.mu.
func (b *bateria) usoDe(id string) *usoMaquina {
	if b.uso == nil {
		b.uso = map[string]*usoMaquina{}
	}
	u := b.uso[id]
	if u == nil {
		u = &usoMaquina{cpuMS: -1}
		b.uso[id] = u
	}
	return u
}

// EnBateria dice si el modo batería está en marcha ahora (con la lectura
// cacheada de la fuente de alimentación).
func (m *Manager) EnBateria() bool {
	m.bat.mu.Lock()
	defer m.bat.mu.Unlock()
	return m.enBateriaAhora()
}

// enBateriaAhora es EnBateria con m.bat.mu tomado. En auto pregunta al
// sistema como mucho cada fuenteCache.
func (m *Manager) enBateriaAhora() bool {
	b := &m.bat
	switch b.cfg.Mode {
	case config.BatteryOn:
		return true
	case config.BatteryOff:
		return false
	}
	ahora := b.now()
	if !b.leidaT.IsZero() && ahora.Sub(b.leidaT) < fuenteCache {
		return b.enBat
	}
	leer := b.enBateriaSO
	if leer == nil {
		leer = enBateriaSO
	}
	v, err := leer()
	if err != nil {
		v = false // sin saberlo, como enchufado: no se pausa nada
	}
	b.enBat, b.leidaT = v, ahora
	return v
}

// intervaloVigilante es cada cuánto da su vuelta el vigilante: every, o
// vigilanteBateria veces más en batería.
func (m *Manager) intervaloVigilante(every time.Duration) time.Duration {
	if m.EnBateria() {
		return every * vigilanteBateria
	}
	return every
}

// Usar apunta que empieza una petición contra la máquina ref y devuelve la
// función que la da por acabada. Le devuelve la QoS si se la había bajado y,
// si despertar y la pausó el modo batería, la reanuda antes de seguir (una
// llamada al invitado no lo haría sola). Antes, si el host durmió, pone en
// hora a las que corren (despertar_host.go). Una ref que no existe no apunta
// nada: el manejador contestará 404.
func (m *Manager) Usar(ctx context.Context, ref string, despertar bool) func() {
	m.revisarSueno(ctx)
	mc, ok := m.Get(ref)
	if !ok {
		return func() {}
	}
	id := mc.ID
	b := &m.bat
	b.mu.Lock()
	u := b.usoDe(id)
	u.enCurso++
	u.ociosaDesde = time.Time{}
	var subir []int
	if u.qosBaja {
		subir, u.qosBaja, u.pids = u.pids, false, nil
	}
	reanudar := despertar && u.pausada && mc.State == api.StatePaused
	b.mu.Unlock()
	for _, p := range subir {
		_ = m.ponerQoS(p, false)
	}
	if reanudar {
		if err := m.reanudarBat(ctx, id); err != nil {
			log.Printf("battery: resuming %s: %v", mc.Name, err)
		}
	}
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		u := b.usoDe(id)
		u.enCurso--
		u.ultimo = b.now()
	}
}

// fuenteEsBateria lee la salida de pmset -g ps: "Now drawing from 'Battery
// Power'" o "'AC Power'" (o "'UPS Power'", que tampoco es batería del Mac).
func fuenteEsBateria(out string) bool {
	return strings.Contains(out, "'Battery Power'")
}

func (m *Manager) ponerQoS(pid int, baja bool) error {
	if m.bat.qos != nil {
		return m.bat.qos(pid, baja)
	}
	return qosProceso(pid, baja)
}

// pausableEnBateria dice si el modo batería puede pausar o tocar mc: corre y
// nadie la usa sin pasar por el daemon (ver arriba).
func pausableEnBateria(mc *api.Machine) bool {
	if mc.State != api.StateRunning || hasLiveShares(mc) {
		return false
	}
	for k := range mc.Labels {
		switch {
		case k == api.LabelPorts, k == api.LabelService, k == api.LabelPool, k == api.LabelPoolReady,
			k == api.LabelGraph, k == api.LabelGraphNode, k == "pool", k == "ephemeral", k == "isolated",
			strings.HasPrefix(k, api.LabelDBPrefix):
			return false
		}
	}
	return true
}

// vueltaBateria es la parte del vigilante del modo batería: con batería mira
// qué máquinas están ociosas, les baja la QoS y pausa las que pasan de
// PauseAfter; al volver al enchufe deshace las dos cosas.
func (m *Manager) vueltaBateria(ctx context.Context) {
	b := &m.bat
	b.mu.Lock()
	bat := m.enBateriaAhora()
	cambio := bat != b.antes
	b.antes = bat
	cfg := b.cfg
	b.mu.Unlock()
	if !bat {
		if cambio {
			m.salirDeBateria(ctx)
		}
		return
	}
	if cambio {
		log.Printf("battery: on battery power; idle machines get background QoS and are paused after %v idle", cfg.PauseAfter)
	}
	vivas := map[string]bool{}
	for _, mc := range m.List() {
		vivas[mc.ID] = true
		if !pausableEnBateria(mc) {
			continue
		}
		m.revisarOciosa(ctx, mc, cfg)
	}
	// Olvidar las que ya no existen.
	b.mu.Lock()
	for id := range b.uso {
		if !vivas[id] {
			delete(b.uso, id)
		}
	}
	b.mu.Unlock()
}

// revisarOciosa es una vuelta del modo batería sobre una máquina running.
func (m *Manager) revisarOciosa(ctx context.Context, mc *api.Machine, cfg BatteryConfig) {
	b := &m.bat
	m.mu.RLock()
	sock := m.socket[mc.ID]
	m.mu.RUnlock()
	if sock == "" {
		return
	}
	leer := b.cpuDe
	if leer == nil {
		leer = cpuInvitado
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	ms, aux, err := leer(cctx, sock)
	cancel()
	if err != nil {
		ms = -1
	}

	b.mu.Lock()
	u := b.usoDe(mc.ID)
	u.pausada = false // corre: si la pausó este modo, ya la despertó alguien
	ahora := b.now()
	antesT := u.cpuT
	ociosa := u.enCurso == 0 && ahora.Sub(u.ultimo) >= min(cfg.PauseAfter, 10*time.Second)
	if ociosa {
		// Sin lectura de CPU (o sin la anterior) no se puede decir que el
		// invitado esté quieto: se espera a la siguiente vuelta.
		if ms < 0 || u.cpuMS < 0 {
			ociosa = false
		} else if dt := ahora.Sub(u.cpuT); dt > 0 {
			ociosa = float64(ms-u.cpuMS) < float64(dt.Milliseconds())*cpuOciosaPct/100
		}
	}
	u.cpuMS, u.cpuT = ms, ahora
	if !ociosa {
		u.ociosaDesde = time.Time{}
		b.mu.Unlock()
		return
	}
	if u.ociosaDesde.IsZero() {
		// Ociosa desde la lectura anterior, que es el principio de la
		// ventana en la que no gastó.
		u.ociosaDesde = antesT
	}
	var bajar []int
	if !u.qosBaja {
		u.qosBaja = true
		for _, p := range []int{mc.PID, aux} {
			if p > 0 {
				bajar = append(bajar, p)
			}
		}
		u.pids = bajar
	}
	pausar := cfg.PauseAfter >= 0 && ahora.Sub(u.ociosaDesde) >= cfg.PauseAfter && ahora.Sub(u.ultimo) >= cfg.PauseAfter
	b.mu.Unlock()

	for _, p := range bajar {
		if err := m.ponerQoS(p, true); err != nil {
			log.Printf("battery: background QoS for %s (pid %d): %v", mc.Name, p, err)
		}
	}
	if !pausar {
		return
	}
	if err := m.pausarBat(ctx, mc.ID); err != nil {
		log.Printf("battery: pausing idle %s: %v", mc.Name, err)
		return
	}
	b.mu.Lock()
	u = b.usoDe(mc.ID)
	// Una petición que llegó mientras se pausaba (M7): Usar la vio running y
	// sin pausada, así que no la reanudó, y entró en una VM que se paraba.
	// Se mira ahora, con la pausa hecha: si hay alguna en curso, o acabó una
	// desde que se decidió pausar, se reanuda ya. Las que lleguen desde aquí
	// ven pausada y la reanudan ellas (Usar, ExecTarget).
	if u.enCurso > 0 || u.ultimo.After(ahora) {
		b.mu.Unlock()
		if err := m.reanudarBat(ctx, mc.ID); err != nil {
			log.Printf("battery: resuming %s after a request came in while pausing it: %v", mc.Name, err)
		}
		return
	}
	u.pausada = true
	// En pausa no gasta y la QoS da igual: se devuelve ya, para que al
	// despertarla (por donde sea) corra como siempre.
	var subir []int
	if u.qosBaja {
		subir = u.pids
	}
	u.qosBaja, u.pids = false, nil
	b.mu.Unlock()
	for _, p := range subir {
		_ = m.ponerQoS(p, false)
	}
	log.Printf("battery: paused %s (idle %v)", mc.Name, ahora.Sub(u.ociosaDesde).Round(time.Second))
}

// salirDeBateria reanuda las máquinas que pausó el modo batería y devuelve la
// QoS a las que se la bajó.
func (m *Manager) salirDeBateria(ctx context.Context) {
	b := &m.bat
	b.mu.Lock()
	var reanudar []string
	var subir []int
	for id, u := range b.uso {
		if u.pausada {
			reanudar = append(reanudar, id)
		}
		if u.qosBaja {
			subir = append(subir, u.pids...)
		}
		u.pausada, u.qosBaja, u.pids, u.ociosaDesde = false, false, nil, time.Time{}
	}
	b.mu.Unlock()
	for _, p := range subir {
		_ = m.ponerQoS(p, false)
	}
	for _, id := range reanudar {
		if mc, ok := m.Get(id); ok && mc.State == api.StatePaused {
			if err := m.reanudarBat(ctx, id); err != nil {
				log.Printf("battery: resuming %s on AC power: %v", mc.Name, err)
			}
		}
	}
	log.Printf("battery: on AC power; resumed %d machine(s) the battery mode had paused", len(reanudar))
}
