package machine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Techo de CPU durante el arranque.
//
// El techo por defecto es media vCPU (defaultCPUPct), y con él el kernel del
// invitado tarda en arrancar casi el doble: ~850 ms medidos de un `kling try`
// de 2,65 s se iban en eso. Un servicio pesado lo nota más: Hindsight (imagen
// Docker con modelos) tardaba 43 s en estar listo con media vCPU y 19 s con un
// núcleo por vCPU. Un techo existe para que una microVM no degrade a sus
// vecinas de forma SOSTENIDA; el arranque es un rato. Así que mientras
// arranca, la microVM corre con un impulso —todas sus vCPU enteras, sin pasar
// de los núcleos del host (topeImpulso)— y vuelve a su techo configurado en
// cuanto termina de arrancar:
//
//   - Si su imagen declara sonda de listo o ganchos (api.GuestReadyProbe), al
//     pasar la sonda (se pregunta como `run -wait-ready`, con WaitReady), como
//     mucho plazoImpulsoListo desde que empezó (KLING_READY_BOOST lo cambia).
//   - Si no declara nada, en cuanto contesta su agente (y a /ready, que dice
//     que no hay nada que esperar).
//   - Tras restaurar (thaw, run -from), en el acto si la memoria ya estaba
//     lista, que es lo normal; si traía ganchos, cuando terminan.
//   - En todo caso, si el agente no contesta en plazoImpulsoCPU (imagen sin
//     agente), si la máquina deja de correr (freeze, rm, VMM caído) o si se
//     cierra el Manager.
//
// KLING_READY_BOOST=0 vuelve al impulso de antes: un núcleo, solo hasta que
// contesta el agente. Un -cpu-pct explícito (Machine.CPUPctFixed) no lleva
// impulso: quien lo pidió manda también durante el arranque.
//
// La admisión no cambia: no reparte CPU (solo memoria y disco), y el impulso
// es un techo de cgroup, no una reserva. Varias máquinas impulsadas a la vez
// se reparten el host por el planificador del kernel, a partes iguales, y
// KLING_MAX_PARALLEL_BOOT sigue acotando cuántas arrancan a la vez.
//
// La regla que no se puede romper: ninguna máquina se queda con el techo de
// arranque. Por eso bajarlo es un impulsoCPU con un bajar() idempotente que
// quien lo crea difiere (defer) en el mismo momento en que sube el techo:
// cualquier salida —éxito, error, pánico— lo baja, salvo que se haya
// ENTREGADO a la goroutine que espera el fin del arranque, que a su vez lo
// baja al volver. Y si el daemon muere en medio, reaplicarTopesCPU lo corrige
// al arrancar de nuevo.
//
// Cada máquina tiene como mucho UN impulso vigente (Manager.impulsos). Uno
// nuevo (un thaw tras un freeze) jubila al anterior, y bajar() solo escribe si
// sigue siendo el vigente: una goroutine tardía no acorta el impulso de otro
// arranque. Sobre un cgroup ya borrado por releaseCPU la escritura falla con
// ENOENT, que se ignora. El mapa no se persiste: lo que enseñan `kling ps` e
// inspect (Machine.CPUBoostPct) es siempre un impulso vivo de este daemon.

// plazoImpulsoCPU es cuánto se espera como mucho al agente antes de bajar el
// techo de todas formas: una imagen sin agente, o un invitado que no llega a
// escucharlo, no se queda con el impulso para siempre.
const plazoImpulsoCPU = 10 * time.Second

// plazoImpulsoListo es cuánto dura como mucho el impulso de una imagen que
// declara sonda de listo, contado desde que empieza. Hindsight, el caso más
// lento medido, está listo en ~19 s con el impulso; el margen es para un host
// cargado. Lo cambia KLING_READY_BOOST (una duración).
const plazoImpulsoListo = 60 * time.Second

// pasoImpulsoCPU es cada cuánto se pregunta si el agente ya escucha. Un
// "connection refused" contesta en microsegundos; el paso acota lo que dura
// el impulso de más.
const pasoImpulsoCPU = 20 * time.Millisecond

// ganchosCPU sustituye piezas del techo de arranque en las pruebas. nil en
// producción.
type ganchosCPU struct {
	// escribir sustituye la escritura de cpu.max en el directorio dir.
	escribir func(dir string, pct int) error
	// agente sustituye a agenteEscucha al esperar el fin del arranque.
	agente func(id string) bool
	// plazo, si no es cero, sustituye a plazoImpulsoCPU.
	plazo time.Duration
	// nucleos, si no es cero, sustituye a runtime.NumCPU().
	nucleos int
}

// politicaImpulso lee KLING_READY_BOOST: vacío, impulso hasta la sonda con
// plazoImpulsoListo; "0" (u "off", "false", "no"), el de antes (un núcleo
// hasta que contesta el agente); una duración ("90s"), el plazo hasta la sonda.
func politicaImpulso() (hastaListo bool, plazo time.Duration) {
	v := strings.TrimSpace(os.Getenv("KLING_READY_BOOST"))
	switch strings.ToLower(v) {
	case "":
		return true, plazoImpulsoListo
	case "0", "off", "false", "no":
		return false, 0
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return true, d
	}
	log.Printf("warning: KLING_READY_BOOST=%q is not 0 or a duration; using %s", v, plazoImpulsoListo)
	return true, plazoImpulsoListo
}

// topeArranque es el techo del impulso de antes (KLING_READY_BOOST=0): un
// núcleo, o el configurado si ya es más.
func topeArranque(pct int) int { return max(pct, 100) }

// topeImpulso es el techo con el que corre una microVM mientras arranca: todas
// sus vCPU enteras, sin pasar de los núcleos del host (por encima, cpu.max no
// limita nada), y nunca menos que el configurado.
func topeImpulso(pct, vcpus, nucleos int) int {
	return max(pct, 100*min(max(vcpus, 1), max(nucleos, 1)))
}

// dirCgroup es el directorio del cgroup de la máquina id.
func (m *Manager) dirCgroup(id string) string {
	return filepath.Join(m.cgroupRoot, "kl-"+id[:8])
}

// escribirCPUMax fija el techo de CPU en el cgroup dir. cpu.max =
// "<cuota> <periodo>" en microsegundos; 100000 = un núcleo completo.
func (m *Manager) escribirCPUMax(dir string, pct int) error {
	if g := m.pruebasCPU; g != nil && g.escribir != nil {
		return g.escribir(dir, pct)
	}
	return os.WriteFile(filepath.Join(dir, "cpu.max"),
		[]byte(fmt.Sprintf("%d 100000", pct*1000)), 0o644)
}

// impulsoCPU es el techo de arranque de UNA microVM y cómo se deshace. Ver el
// comentario del principio del fichero.
type impulsoCPU struct {
	m          *Manager
	id         string
	pct        int  // el techo configurado, el que tiene que quedar
	tope       int  // el de arranque; == pct si no hay impulso
	hastaListo bool // esperar a la sonda de listo, no solo al agente
	plazoListo time.Duration
	inicio     time.Time
	una        sync.Once
	entregado  bool // solo lo toca la goroutine que creó el impulso
}

// nuevoImpulso prepara el impulso de la máquina id, de vcpus vCPU y techo
// configurado pct (fijo si lo pidió quien la arranca: entonces no hay
// impulso). No escribe nada: el techo de arranque (impulso.tope) lo pone quien
// crea el cgroup (cgroupParaLanzar o limitCPU). Quien lo llama difiere fin()
// justo después.
func (m *Manager) nuevoImpulso(id string, pct, vcpus int, fijo bool) *impulsoCPU {
	i := &impulsoCPU{m: m, id: id, pct: pct, tope: pct, inicio: time.Now()}
	if m.cgroupRoot == "" || fijo {
		return i
	}
	i.hastaListo, i.plazoListo = politicaImpulso()
	if i.hastaListo {
		nucleos := runtime.NumCPU()
		if g := m.pruebasCPU; g != nil && g.nucleos > 0 {
			nucleos = g.nucleos
		}
		i.tope = topeImpulso(pct, vcpus, nucleos)
	} else {
		i.tope = topeArranque(pct)
	}
	if i.tope != pct {
		m.impulsosMu.Lock()
		if m.impulsos == nil {
			m.impulsos = map[string]*impulsoCPU{}
		}
		m.impulsos[id] = i // jubila al de un arranque anterior
		m.impulsosMu.Unlock()
	}
	return i
}

// impulsoVigente es el techo de arranque que lleva ahora la máquina id, o 0.
// Para Machine.CPUBoostPct en List y Get.
// Solo de una que corre: una congelada o parada a mitad del impulso ya no lo
// gasta, aunque su goroutine tarde un paso en darse cuenta.
func (m *Manager) impulsoVigente(id string, estado api.State) int {
	if estado != api.StateRunning {
		return 0
	}
	m.impulsosMu.Lock()
	defer m.impulsosMu.Unlock()
	if i := m.impulsos[id]; i != nil {
		return i.tope
	}
	return 0
}

// bajar aplica el techo configurado y publica por qué. Idempotente y seguro
// desde cualquier goroutine; sin impulso, o si ya lo jubiló otro arranque de
// la misma máquina, no escribe nada.
func (i *impulsoCPU) bajar(motivo string) {
	i.una.Do(func() {
		if i.tope == i.pct {
			return
		}
		m := i.m
		// La escritura va bajo impulsosMu: un arranque nuevo se registra bajo
		// el mismo candado antes de escribir su techo, así que esta no puede
		// pisarlo.
		m.impulsosMu.Lock()
		vigente := m.impulsos[i.id] == i
		if vigente {
			delete(m.impulsos, i.id)
			err := m.escribirCPUMax(m.dirCgroup(i.id), i.pct)
			if err != nil && !os.IsNotExist(err) {
				log.Printf("warning: %s: could not restore cpu.max after boot: %v", shortID(i.id), err)
			}
		}
		m.impulsosMu.Unlock()
		if vigente && m.bus != nil {
			m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvBoostEnded, ID: i.id,
				Message: fmt.Sprintf("cpu boost %d%% -> %d%% after %s: %s",
					i.tope, i.pct, time.Since(i.inicio).Round(time.Millisecond), motivo)})
		}
	})
}

// fin es lo que se difiere: baja el techo, salvo que ya se haya entregado a
// la goroutine de entregar(), que es entonces quien lo baja.
func (i *impulsoCPU) fin() {
	if !i.entregado {
		i.bajar("start did not finish")
	}
}

// entregar deja el impulso en manos de una goroutine que lo baja cuando la
// máquina termina de arrancar (ver el principio del fichero). Para Run, que
// devuelve la máquina en cuanto el VMM arranca y antes de que el invitado
// termine de hacerlo: es justo ese arranque el que el impulso acelera.
func (i *impulsoCPU) entregar() { i.entregarDesde(false) }

// entregarRestaurada es entregar() tras una restauración (thaw, run -from):
// el agente ya contestó o no lo hay. listo es lo que contestó al resync (nil:
// sin agente, o uno anterior a /ready). Si ya está listo y no trae ganchos —lo
// normal: el "listo" viaja en la memoria—, baja aquí mismo, sin goroutine.
func (i *impulsoCPU) entregarRestaurada(listo *api.GuestReady) {
	if i.tope == i.pct {
		return
	}
	switch {
	case !i.hastaListo:
		i.bajar("guest agent answered")
	case listo == nil:
		i.bajar("restored, no ready probe to wait for")
	case listo.Ready && !listo.HasHooks:
		i.bajar("restored ready")
	default:
		i.entregarDesde(true)
	}
}

func (i *impulsoCPU) entregarDesde(trasAgente bool) {
	if i.tope == i.pct {
		return // no hay nada que bajar: que fin() lo resuelva sin goroutine
	}
	i.entregado = true
	go func() {
		i.bajar(i.m.esperarFinArranque(i, trasAgente))
	}()
}

// esperarFinArranque vuelve, con el motivo, cuando la máquina del impulso i
// termina de arrancar o cuando ya no tiene sentido esperarla: plazo agotado,
// máquina que ya no corre (parada, borrada, congelada) o Manager cerrado.
// trasAgente salta la espera al agente.
func (m *Manager) esperarFinArranque(i *impulsoCPU, trasAgente bool) string {
	id := i.id
	plazo := plazoImpulsoCPU
	escucha := func(id string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return m.agenteEscucha(ctx, id)
	}
	if g := m.pruebasCPU; g != nil {
		if g.plazo > 0 {
			plazo = g.plazo
		}
		if g.agente != nil {
			escucha = g.agente
		}
	}
	if !trasAgente {
		limite := time.NewTimer(plazo)
		defer limite.Stop()
		for {
			m.mu.RLock()
			mc := m.byID[id]
			corre := mc != nil && mc.State == api.StateRunning
			m.mu.RUnlock()
			if !corre {
				return "machine is no longer running"
			}
			if escucha(id) {
				break
			}
			select {
			case <-limite.C:
				return fmt.Sprintf("no guest agent after %s", plazo)
			case <-m.quit:
				return "daemon closing"
			case <-time.After(pasoImpulsoCPU):
			}
		}
		if !i.hastaListo {
			return "guest agent answered"
		}
	}
	// El agente contestó: ahora, la sonda. Se pregunta como `run -wait-ready`
	// (WaitReady): Machine.Ready no basta, porque "" es a la vez "no declara
	// nada" y "aún nadie lo ha mirado". Una imagen sin sonda contesta listo a
	// la primera; un agente anterior a /ready, también.
	ctx, cancel := context.WithTimeout(context.Background(), max(i.plazoListo-time.Since(i.inicio), time.Millisecond))
	defer cancel()
	go func() {
		select {
		case <-m.quit:
			cancel()
		case <-ctx.Done():
		}
	}()
	res, err := m.WaitReady(ctx, id, OpcionesListo{Plazo: i.plazoListo})
	switch {
	case err == nil && res.Ready == api.ReadyYes:
		return "ready probe passed"
	case err == nil:
		return "guest agent answered, no ready probe"
	case errors.Is(err, ErrNoMachine) || errors.Is(err, ErrNotRunning):
		return "machine is no longer running"
	case res.Ready == api.ReadyFailed:
		return "post-restore hooks failed"
	case ctx.Err() != nil && isClosed(m.quit):
		return "daemon closing"
	case ctx.Err() != nil:
		return fmt.Sprintf("not ready after %s", i.plazoListo)
	}
	return err.Error()
}

// reaplicarTopesCPU devuelve a su techo configurado toda máquina viva al
// arrancar el daemon. Si el anterior murió con un arranque en marcha, la
// goroutine que iba a bajarle el techo murió con él, y el cgroup —que sí
// sobrevive— seguiría con el de arranque para siempre.
func (m *Manager) reaplicarTopesCPU() {
	if m.cgroupRoot == "" {
		return
	}
	type tope struct {
		id  string
		pct int
	}
	var topes []tope
	m.mu.RLock()
	for id, mc := range m.byID {
		if mc.PID > 0 && mc.CPUPct > 0 && len(id) >= 8 {
			topes = append(topes, tope{id, mc.CPUPct})
		}
	}
	m.mu.RUnlock()
	for _, t := range topes {
		err := m.escribirCPUMax(m.dirCgroup(t.id), t.pct)
		if err != nil && !os.IsNotExist(err) {
			log.Printf("warning: %s: could not reapply cpu.max: %v", shortID(t.id), err)
		}
	}
}
