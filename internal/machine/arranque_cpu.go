package machine

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Techo de CPU durante el arranque.
//
// El techo por defecto es media vCPU (defaultCPUPct), y con él el kernel del
// invitado tarda en arrancar casi el doble: ~850 ms medidos de un `kling try`
// de 2,65 s se iban en eso. Un techo existe para que una microVM no degrade a
// sus vecinas de forma SOSTENIDA; el arranque dura un segundo. Así que
// mientras arranca, la microVM corre con max(CPUPct, 100) —un núcleo
// completo— y, en cuanto su agente contesta, con el techo configurado.
//
// La regla que no se puede romper: ninguna máquina se queda con el techo de
// arranque. Por eso bajarlo es un impulsoCPU con un bajar() idempotente que
// quien lo crea difiere (defer) en el mismo momento en que sube el techo:
// cualquier salida —éxito, error, pánico— lo baja, salvo que se haya
// ENTREGADO a la goroutine que espera al agente, que a su vez lo baja en su
// propio defer (al contestar el agente, al agotar plazoImpulsoCPU, al dejar de
// correr la máquina o al cerrarse el Manager). Y si el daemon muere en medio,
// reaplicarTopesCPU lo corrige al arrancar de nuevo.
//
// Bajar el techo no toma el cerrojo de ciclo de vida: solo BAJA, nunca sube,
// así que una escritura tardía sobre la máquina (tras un Freeze→Thaw que ya
// la volvió a subir) como mucho acorta un impulso ajeno, y sobre un cgroup ya
// borrado por releaseCPU falla con ENOENT, que se ignora. Solo crearCgroup
// escribe el techo de arranque, y siempre seguido de su propio impulsoCPU.

// plazoImpulsoCPU es cuánto se espera como mucho al agente antes de bajar el
// techo de todas formas: una imagen sin agente, o un invitado que no llega a
// escucharlo, no se queda un núcleo entero para siempre. Con margen sobre el
// arranque de un host cargado, pero corto frente a lo que protege el techo.
const plazoImpulsoCPU = 10 * time.Second

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
}

// topeArranque es el techo con el que corre una microVM mientras arranca.
func topeArranque(pct int) int { return max(pct, 100) }

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
	m         *Manager
	id        string
	pct       int // el techo configurado, el que tiene que quedar
	una       sync.Once
	entregado bool // solo lo toca la goroutine que creó el impulso
}

// nuevoImpulso prepara el impulso de la máquina id, cuyo techo configurado es
// pct. No escribe nada: el techo de arranque lo pone quien crea el cgroup
// (cgroupParaLanzar o limitCPU con topeArranque(pct)). Quien lo llama difiere
// fin() justo después.
func (m *Manager) nuevoImpulso(id string, pct int) *impulsoCPU {
	return &impulsoCPU{m: m, id: id, pct: pct}
}

// bajar aplica el techo configurado. Idempotente y seguro desde cualquier
// goroutine; sin cgroups, o si el techo de arranque ya era el configurado, no
// hace nada.
func (i *impulsoCPU) bajar() {
	i.una.Do(func() {
		if i.m.cgroupRoot == "" || topeArranque(i.pct) == i.pct {
			return
		}
		err := i.m.escribirCPUMax(i.m.dirCgroup(i.id), i.pct)
		if err != nil && !os.IsNotExist(err) {
			log.Printf("warning: %s: could not restore cpu.max after boot: %v", shortID(i.id), err)
		}
	})
}

// fin es lo que se difiere: baja el techo, salvo que ya se haya entregado a
// la goroutine de entregar(), que es entonces quien lo baja.
func (i *impulsoCPU) fin() {
	if !i.entregado {
		i.bajar()
	}
}

// entregar deja el impulso en manos de una goroutine que lo baja cuando el
// agente de la máquina contesta, y en todo caso al agotarse el plazo, al
// dejar de correr la máquina o al cerrarse el Manager. Para Run, que devuelve
// la máquina en cuanto el VMM arranca y antes de que el invitado termine de
// hacerlo: es justo ese arranque del kernel el que el impulso acelera.
func (i *impulsoCPU) entregar() {
	if i.m.cgroupRoot == "" || topeArranque(i.pct) == i.pct {
		return // no hay nada que bajar: que fin() lo resuelva sin goroutine
	}
	i.entregado = true
	go func() {
		defer i.bajar()
		i.m.esperarFinArranque(i.id)
	}()
}

// esperarFinArranque vuelve cuando el agente de la máquina id contesta, o
// cuando ya no tiene sentido esperarlo: plazo agotado, máquina que ya no corre
// (parada, borrada, congelada) o Manager cerrado.
func (m *Manager) esperarFinArranque(id string) {
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
	limite := time.NewTimer(plazo)
	defer limite.Stop()
	for {
		m.mu.RLock()
		mc := m.byID[id]
		corre := mc != nil && mc.State == api.StateRunning
		m.mu.RUnlock()
		if !corre || escucha(id) {
			return
		}
		select {
		case <-limite.C:
			return
		case <-m.quit:
			return
		case <-time.After(pasoImpulsoCPU):
		}
	}
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
