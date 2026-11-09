package machine

// LA PRESIÓN DE MEMORIA DEL HOST.
//
// Cuando macOS se queda sin memoria, jetsam mata procesos para recuperarla, y
// los VMM (kling-hv, kling-vz) son de los que más ocupan: la máquina moría sin
// aviso, con lo que tuviera sin guardar. El daemon ya decía que no a arrancar
// otra con poca memoria (admision.go, checkPresionPlataforma), pero no hacía
// nada por las que ya corrían.
//
// Ahora el vigilante lee el nivel de presión del sistema en cada vuelta
// (macOS: kern.memorystatus_vm_pressure_level, 1 normal, 2 aviso, 4 crítico;
// Linux: PSI "some avg10" de /proc/pressure/memory, ver nivelPresionPSI) y:
//
//   - en aviso, aprieta el globo (squeeze) de las máquinas ociosas: devuelve
//     al host la RAM que su invitado tiene libre, sin parar nada;
//   - en crítico, además congela (freeze) la ociosa que más ocupa: su RAM va
//     a disco y la siguiente petición la descongela.
//
// Ociosa es lo mismo que para el modo batería, y más estricto: corre, nadie la
// usa sin pasar por el daemon (pausableEnBateria: ni puertos publicados, ni
// servicios, fondos, grafos, kling db o carpetas vivas), sin peticiones en
// curso ni terminadas hace menos de IdleAfter, sin sesiones del broker de
// aristas, y con su VMM gastando menos de cpuOciosaPct de un núcleo entre dos
// vueltas. Nunca se toca una con un exec, un shell o un servicio en uso.
//
// Por orden: primero las de más huella y, a igualdad, las que llevan más
// tiempo sin uso. Y con freno, para no apretar en bucle mientras el nivel no
// baja: como mucho presionApretones apretones y presionCongelaciones
// congelaciones por vuelta que actúa, presionEnfriamiento entre dos vueltas
// que actúan (lo que tarda el sistema en notar lo devuelto), presionReapretar
// antes de volver a apretar la misma máquina, e histéresis al bajar de nivel
// (presionBajada). daemon.memory_pressure=off lo apaga.

import (
	"context"
	"errors"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
)

// Niveles de presión, los de kern.memorystatus_vm_pressure_level
// (kVMPressureNormal, Warning, Critical). 0 = no se sabe.
const (
	NivelPresionNormal  = 1
	NivelPresionAviso   = 2
	NivelPresionCritico = 4
)

const (
	// presionOciosaDef es MemPressureConfig.IdleAfter por defecto.
	presionOciosaDef = 2 * time.Minute
	// presionBajada es lo que tiene que aguantar la fuente por debajo del
	// nivel en vigor para darlo por bajado: el de macOS oscila entre aviso y
	// normal en segundos cuando el sistema está al borde.
	presionBajada = 30 * time.Second
	// presionEnfriamiento separa dos vueltas que actúan: lo que se devuelve
	// tarda en verse en el nivel, y actuar otra vez antes sería castigar a
	// más máquinas por la misma presión.
	presionEnfriamiento = 30 * time.Second
	// presionReapretar: una máquina apretada no se vuelve a apretar antes.
	// Una ociosa no vuelve a llenar su caché sola, y apretarla de nuevo no
	// devolvería nada.
	presionReapretar = 10 * time.Minute
	// Topes por vuelta que actúa.
	presionApretones     = 2
	presionCongelaciones = 1
)

// MemPressureConfig es la configuración de la reacción a la presión.
type MemPressureConfig struct {
	Mode      string        // config.MemoryPressureAuto (o "") u Off
	IdleAfter time.Duration // 0 = presionOciosaDef
}

func (c MemPressureConfig) activa() bool { return c.Mode != config.MemoryPressureOff }

// lecturaCPU es la CPU del VMM de una máquina en una vuelta.
type lecturaCPU struct {
	cpu time.Duration
	t   time.Time
}

// presionMemoria es el estado de la reacción a la presión del Manager.
type presionMemoria struct {
	mu  sync.Mutex
	cfg MemPressureConfig
	// puesta: SetMemPressure ya se llamó. Sin ella no se hace nada: los
	// Manager de las pruebas no leen el nivel del Mac en el que corren.
	puesta bool

	nivel        int       // en vigor, con histéresis; 0 = aún no se leyó
	bajaDesde    time.Time // desde cuándo la fuente da menos que nivel
	errorVisto   bool      // la fuente falló y ya se avisó
	ultimaAccion time.Time
	actuando     bool // aplicarPresion en curso (va en su goroutine)
	cpu          map[string]lecturaCPU

	// Puntos de inyección de las pruebas; nil = los de verdad.
	fuente   func() (int, error)
	ahora    func() time.Time
	cpuDe    func(pid int, sock string) (time.Duration, bool)
	huellaDe func(pid int, sock string) int64
	apretar  func(ctx context.Context, id string) (int, error)
	congelar func(ctx context.Context, id string, sigue func(*api.Machine) bool) error
}

// SetMemPressure fija la configuración. Se llama al arrancar el daemon.
func (m *Manager) SetMemPressure(c MemPressureConfig) {
	if c.IdleAfter <= 0 {
		c.IdleAfter = presionOciosaDef
	}
	m.pres.mu.Lock()
	m.pres.cfg, m.pres.puesta = c, true
	m.pres.mu.Unlock()
}

func (p *presionMemoria) now() time.Time {
	if p.ahora != nil {
		return p.ahora()
	}
	return time.Now()
}

// normalizarNivel lleva una lectura cualquiera a uno de los tres niveles.
func normalizarNivel(v int) int {
	switch {
	case v >= NivelPresionCritico:
		return NivelPresionCritico
	case v >= NivelPresionAviso:
		return NivelPresionAviso
	default:
		return NivelPresionNormal
	}
}

// nombreNivel es el nivel para el log.
func nombreNivel(n int) string {
	switch n {
	case NivelPresionCritico:
		return "critical"
	case NivelPresionAviso:
		return "warn"
	case NivelPresionNormal:
		return "normal"
	}
	return "unknown"
}

// actualizar mete una lectura en la histéresis: subir es inmediato, bajar
// solo tras presionBajada seguidos por debajo. Con p.mu. Devuelve si cambió
// el nivel en vigor.
func (p *presionMemoria) actualizar(leido int, ahora time.Time) bool {
	leido = normalizarNivel(leido)
	if leido >= p.nivel {
		p.bajaDesde = time.Time{}
		if leido > p.nivel {
			p.nivel = leido
			return true
		}
		return false
	}
	if p.bajaDesde.IsZero() {
		p.bajaDesde = ahora
	}
	if ahora.Sub(p.bajaDesde) < presionBajada {
		return false
	}
	p.nivel, p.bajaDesde = leido, time.Time{}
	return true
}

// candidatoPresion es una máquina ociosa que se puede apretar o congelar.
type candidatoPresion struct {
	ID, Name    string
	HuellaMiB   int64
	SinUsoDesde time.Time // la última petición o, si no hubo, el arranque
	Ultima      time.Time // la última petición (Actividad), para revalidar
	Compartida  bool      // MemShared: apretarla sube la memoria del host
}

// planPresion es lo que toca hacer en una vuelta.
type planPresion struct {
	nivel    int
	apretar  []candidatoPresion
	congelar []candidatoPresion
}

func (p planPresion) vacio() bool { return len(p.apretar) == 0 && len(p.congelar) == 0 }

// planificarPresion decide, con el nivel en vigor y las ociosas de la vuelta,
// a quién apretar y a quién congelar. apretadaEn es cuándo se apretó cada
// una por última vez (por quien fuera: también makeRoom). Pura, para probarla
// sin máquinas.
func planificarPresion(nivel int, cands []candidatoPresion, apretadaEn map[string]time.Time, ahora time.Time) planPresion {
	plan := planPresion{nivel: nivel}
	if nivel < NivelPresionAviso || len(cands) == 0 {
		return plan
	}
	orden := append([]candidatoPresion(nil), cands...)
	sort.SliceStable(orden, func(i, j int) bool {
		if orden[i].HuellaMiB != orden[j].HuellaMiB {
			return orden[i].HuellaMiB > orden[j].HuellaMiB
		}
		if !orden[i].SinUsoDesde.Equal(orden[j].SinUsoDesde) {
			return orden[i].SinUsoDesde.Before(orden[j].SinUsoDesde)
		}
		return orden[i].ID < orden[j].ID
	})
	congelada := map[string]bool{}
	if nivel >= NivelPresionCritico {
		for _, c := range orden {
			if len(plan.congelar) == presionCongelaciones {
				break
			}
			plan.congelar = append(plan.congelar, c)
			congelada[c.ID] = true
		}
	}
	for _, c := range orden {
		if len(plan.apretar) == presionApretones {
			break
		}
		if congelada[c.ID] || c.Compartida {
			continue
		}
		if t, ok := apretadaEn[c.ID]; ok && ahora.Sub(t) < presionReapretar {
			continue
		}
		plan.apretar = append(plan.apretar, c)
	}
	return plan
}

// vueltaPresion es la parte del vigilante que lee el nivel y decide. Lo que
// decide lo hace aplicarPresion (en su goroutine desde el vigilante: congelar
// tarda, y la vuelta no debe esperarlo).
func (m *Manager) vueltaPresion() planPresion {
	p := &m.pres
	p.mu.Lock()
	defer p.mu.Unlock()
	cfg := p.cfg
	if cfg.IdleAfter <= 0 {
		cfg.IdleAfter = presionOciosaDef
	}
	if !p.puesta || !cfg.activa() {
		p.nivel, p.cpu = 0, nil
		tel.presionNivel.Store(0)
		return planPresion{}
	}
	fuente := p.fuente
	if fuente == nil {
		fuente = nivelPresionSO
	}
	leido, err := fuente()
	if err != nil {
		if !p.errorVisto && !errors.Is(err, errSinPresion) {
			log.Printf("memory pressure: cannot read the host level: %v", err)
		}
		p.errorVisto = true
		return planPresion{}
	}
	p.errorVisto = false
	ahora := p.now()
	antes := p.nivel
	if p.actualizar(leido, ahora) {
		log.Printf("memory pressure: %s (was %s)", nombreNivel(p.nivel), nombreNivel(antes))
	}
	tel.presionNivel.Store(int64(p.nivel))
	if p.nivel < NivelPresionAviso {
		// Sin presión no se lee la CPU de nadie: a la primera vuelta con
		// presión se toma la referencia y a la segunda ya se sabe quién está
		// quieta (20 s con el vigilante de 10 s).
		p.cpu = nil
		return planPresion{}
	}
	cands := m.ociosasPresion(p, cfg, ahora)
	if p.actuando || ahora.Sub(p.ultimaAccion) < presionEnfriamiento {
		return planPresion{}
	}
	m.mu.RLock()
	apretadaEn := make(map[string]time.Time, len(m.squeezedAt))
	for id, t := range m.squeezedAt {
		apretadaEn[id] = t
	}
	m.mu.RUnlock()
	plan := planificarPresion(p.nivel, cands, apretadaEn, ahora)
	if !plan.vacio() {
		p.ultimaAccion = ahora
		p.actuando = true
	}
	return plan
}

// ociosasPresion son las máquinas que se pueden tocar. Lee la CPU de todas las
// que corren y apunta la lectura para la vuelta siguiente. Con p.mu.
func (m *Manager) ociosasPresion(p *presionMemoria, cfg MemPressureConfig, ahora time.Time) []candidatoPresion {
	cpuDe := p.cpuDe
	if cpuDe == nil {
		cpuDe = func(pid int, sock string) (time.Duration, bool) {
			u, ok := cpuVMM(pid, sock)
			return u.total, ok
		}
	}
	huellaDe := p.huellaDe
	if huellaDe == nil {
		huellaDe = memoriaVMM
	}
	type viva struct {
		mc   *api.Machine
		sock string
	}
	var vivas []viva
	m.mu.RLock()
	for _, mc := range m.byID {
		if mc.State == api.StateRunning && mc.PID > 0 {
			vivas = append(vivas, viva{mc, m.socket[mc.ID]})
		}
	}
	m.mu.RUnlock()

	nuevas := make(map[string]lecturaCPU, len(vivas))
	var out []candidatoPresion
	for _, v := range vivas {
		mc := v.mc
		cpu, ok := cpuDe(mc.PID, v.sock)
		prev, hayPrev := p.cpu[mc.ID]
		if ok {
			nuevas[mc.ID] = lecturaCPU{cpu, ahora}
		}
		if !pausableEnBateria(mc) {
			continue
		}
		enCurso, ultima, sesiones := m.Actividad(mc.ID)
		if enCurso > 0 || sesiones > 0 {
			continue
		}
		desde := ultima
		if mc.StartedAt != nil && mc.StartedAt.After(desde) {
			desde = *mc.StartedAt
		}
		if ahora.Sub(desde) < cfg.IdleAfter {
			continue
		}
		// Sin dos lecturas de CPU no se puede decir que el invitado esté
		// quieto: se espera a la vuelta siguiente.
		if !ok || !hayPrev {
			continue
		}
		dt := ahora.Sub(prev.t)
		if dt <= 0 || float64(cpu-prev.cpu) >= float64(dt)*cpuOciosaPct/100 {
			continue
		}
		out = append(out, candidatoPresion{
			ID: mc.ID, Name: mc.Name,
			HuellaMiB:   huellaDe(mc.PID, v.sock),
			SinUsoDesde: desde, Ultima: ultima,
			Compartida: mc.MemShared,
		})
	}
	p.cpu = nuevas
	return out
}

// aplicarPresion hace el plan: congela y aprieta, deja constancia en el log y
// en /metrics, y libera la vuelta siguiente.
func (m *Manager) aplicarPresion(ctx context.Context, plan planPresion) {
	p := &m.pres
	defer func() {
		p.mu.Lock()
		p.actuando = false
		p.mu.Unlock()
	}()
	congelar := p.congelar
	if congelar == nil {
		congelar = func(ctx context.Context, id string, sigue func(*api.Machine) bool) error {
			_, err := m.freezeSi(ctx, id, sigue)
			return err
		}
	}
	apretar := p.apretar
	if apretar == nil {
		apretar = m.apretarPorPresion
	}
	nivel := nombreNivel(plan.nivel)
	for _, c := range plan.congelar {
		// Se revalida con el cerrojo tomado: entre la foto y aquí pudo
		// llegarle una petición, y congelarla entonces era cortársela.
		sigue := func(cur *api.Machine) bool {
			if cur.State != api.StateRunning {
				return false
			}
			enCurso, ultima, sesiones := m.Actividad(cur.ID)
			return enCurso == 0 && sesiones == 0 && !ultima.After(c.Ultima)
		}
		if err := congelar(ctx, c.ID, sigue); err != nil {
			if !errors.Is(err, errYaNoToca) {
				log.Printf("memory pressure (%s): freezing idle %s: %v", nivel, c.Name, err)
			}
			continue
		}
		tel.presionCongela.Add(1)
		log.Printf("memory pressure (%s): froze idle %s (%d MiB, unused for %v)",
			nivel, c.Name, c.HuellaMiB, p.now().Sub(c.SinUsoDesde).Round(time.Second))
	}
	for _, c := range plan.apretar {
		mib, err := apretar(ctx, c.ID)
		if err != nil {
			log.Printf("memory pressure (%s): squeezing idle %s: %v", nivel, c.Name, err)
			continue
		}
		tel.presionAprieta.Add(1)
		tel.presionDevueltaMiB.Add(int64(mib))
		log.Printf("memory pressure (%s): squeezed idle %s, %d MiB back to the host (%d MiB, unused for %v)",
			nivel, c.Name, mib, c.HuellaMiB, p.now().Sub(c.SinUsoDesde).Round(time.Second))
	}
}

// apretarPorPresion aprieta el globo de id sin esperar a su cerrojo (una
// ocupada se salta, como en makeRoom) y apunta cuándo, para el freno.
func (m *Manager) apretarPorPresion(ctx context.Context, id string) (int, error) {
	soltar, ok := m.tryLock(id)
	if !ok {
		return 0, errors.New("busy, skipped")
	}
	res, err := m.squeezeLocked(ctx, id, id, false, balloonSqueezeMarginMiB, true)
	soltar()
	m.mu.Lock()
	if m.squeezedAt == nil {
		m.squeezedAt = map[string]time.Time{}
	}
	m.squeezedAt[id] = m.pres.now()
	m.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return res.ReclaimedMiB, nil
}

// errSinPresion: el host no publica un nivel de presión (Linux sin PSI). No
// se avisa: no es un fallo.
var errSinPresion = errors.New("the host does not report memory pressure")

// vigilarPresion es lo que el vigilante llama en cada vuelta.
func (m *Manager) vigilarPresion(ctx context.Context) {
	plan := m.vueltaPresion()
	if plan.vacio() {
		return
	}
	// Contada como vigilante: Close la espera (ver lanzarVigilante).
	m.lanzarVigilante(ctx, func(ctx context.Context) { m.aplicarPresion(ctx, plan) })
}
