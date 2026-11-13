package machine

// El almacén lleno no puede llegar al invitado como un EIO.
//
// Un clon por reflink no ocupa nada al nacer, y la cuota de cada instancia es
// una cota de crecimiento, no una reserva (docs/cow.md): N instancias que
// escriben a la vez pueden llenar el almacén aunque cada una cupiese al
// admitirla. Cuando pasa, el VMM recibe ENOSPC del host y se lo da al invitado
// como un error de E/S: ext4 aborta el journal y Postgres hace PANIC. Medido
// en el laboratorio: `kling db fork -n 8` dijo OK y las copias murieron al
// escribir, sin que kling dijera nada del almacén.
//
// Tres defensas:
//
//  1. Admisión (clonarInstancia, comprobarAlmacenPara): ni se clona ni se
//     despierta una instancia del almacén si no quedan libreMinimaAlmacen
//     asignables de verdad (cow_asignable.go).
//  2. El vigilante (vigilarAlmacen): mira lo asignable, más a menudo cuanto
//     menos queda (intervaloVigiaAlmacen); por debajo de marcaPausaAlmacen PAUSA las instancias que
//     viven en el almacén (sus vCPU paran, nada más escribe) y les pone
//     Hold = api.HoldStoreFull. Una pausada no pierde nada: en cuanto vuelve
//     a haber libreMinimaAlmacen (kling cow grow, o al borrar otras), las
//     reanuda solo. Una base de datos que espera es recuperable; una que vio
//     EIO, no siempre.
//  3. Si aun así el invitado ve errores de disco, se anotan en la máquina
//     (errores_disco.go) y lo dicen `kling ps` y `kling db doctor`.
//
// Sin almacén (macOS siempre; Linux sin daemon.cow) no hay cuota del núcleo
// sobre el overlay de cada instancia, y un VMM comprometido podía alargarlo
// (ftruncate, escribir más allá del final) hasta llenar el disco del host. Ahí
// el vigilante es otro (vigilarOverlays): mira el fichero de cada máquina en
// marcha y para la que pasa de lo que su invitado puede direccionar.

import (
	"context"
	"errors"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// El vigilante mira más a menudo cuanto menos queda: las escrituras del
// invitado van a la caché de páginas del host a velocidad de memoria, y cinco
// copias de Postgres escribiendo a la vez se comieron 128 MiB entre dos
// vueltas de 250 ms en el laboratorio. Con velocidadLlenado de supuesto, entre
// dos vueltas no se puede gastar más de una cuarta parte de lo que queda por
// encima de la marca. Un statfs y un ioctl cuestan microsegundos: 100 por
// segundo con el almacén casi lleno no se notan.
const (
	vigiaAlmacenMin  = 10 * time.Millisecond
	vigiaAlmacenMax  = time.Second
	velocidadLlenado = 4 << 30 // bytes por segundo
)

// tiempoParaPausar es lo que se supone que tardan en quedar pausadas todas las
// instancias desde que el vigilante decide: mientras, siguen escribiendo. La
// marca de pausa crece con lo que se está gastando por segundo (vigia.marca).
const tiempoParaPausar = 500 * time.Millisecond

// vigia es el estado del vigilante del almacén entre vueltas.
type vigia struct {
	lleno bool      // la vuelta anterior lo vio por debajo de la marca
	t     time.Time // cuándo midió la última vez
	libre int64     // lo asignable que vio
	tasa  float64   // bytes por segundo que se gastan, con memoria (decae en ~2 s)
	sitio time.Time // desde cuándo hay sitio para reanudar (cero: no lo hay)

	// La tasa se mide en ventanas de al menos ventanaTasa: en 10 ms, lo
	// libre de un Btrfs salta con las reservas de la caché y daba 20 GB/s.
	refT     time.Time
	refLibre int64
}

// esperaReanudar es cuánto tiene que durar el sitio antes de reanudar: lo
// libre de un Btrfs sube y baja mientras se asientan las reservas de lo ya
// escrito, y reanudar con el primer pico hacía que pausase otra vez al segundo.
const esperaReanudar = 5 * time.Second

// ventanaTasa es la ventana mínima para medir la tasa de llenado.
const ventanaTasa = 100 * time.Millisecond

// medir anota una medida y actualiza la tasa de llenado.
func (v *vigia) medir(ahora time.Time, libre int64) {
	if v.refT.IsZero() {
		v.refT, v.refLibre = ahora, libre
	} else if dt := ahora.Sub(v.refT); dt >= ventanaTasa {
		inst := max(float64(v.refLibre-libre)/dt.Seconds(), 0)
		v.tasa = max(inst, v.tasa*math.Exp(-dt.Seconds()/2))
		v.refT, v.refLibre = ahora, libre
	}
	v.t, v.libre = ahora, libre
}

// marca es por debajo de cuánto se pausa: marcaPausaAlmacen más lo que da
// tiempo a escribir mientras se pausa al ritmo de ahora.
func (v *vigia) marca() int64 {
	return marcaPausaAlmacen + int64(v.tasa*tiempoParaPausar.Seconds())
}

// espera es cuánto esperar a la siguiente vuelta: entre dos vueltas no se
// puede gastar más de una cuarta parte de lo que queda por encima de la
// marca, al ritmo de ahora o, como mínimo, a velocidadLlenado.
func (v *vigia) espera() time.Duration {
	margen := max(v.libre-v.marca(), 0)
	d := time.Duration(float64(margen) / 4 / max(v.tasa, velocidadLlenado) * float64(time.Second))
	return min(max(d, vigiaAlmacenMin), vigiaAlmacenMax)
}

// vigilarAlmacen es el bucle del vigilante del almacén (ver arriba).
func (m *Manager) vigilarAlmacen(ctx context.Context) {
	if m.alm == nil {
		m.vigilarOverlays(ctx)
		return
	}
	v := &vigia{}
	espera := vigiaAlmacenMax
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(espera):
			espera = vigiaAlmacenMax
			if m.vueltaVigiaAlmacen(ctx, v) {
				espera = v.espera()
			}
		}
	}
}

// vueltaVigiaAlmacen es una vuelta del vigilante del almacén. Dice si midió
// (sin almacén montado no hay nada que vigilar).
func (m *Manager) vueltaVigiaAlmacen(ctx context.Context, v *vigia) bool {
	if !m.alm.estaListo() {
		*v = vigia{}
		return false
	}
	_, libre, err := m.alm.libreDentro()
	if err != nil {
		return false
	}
	v.medir(time.Now(), libre)
	switch marca := v.marca(); {
	case libre < marca:
		pausadas := m.pausarPorAlmacen(ctx)
		// Sin nada que pausar, se avisa solo si de verdad está lleno: por la
		// tasa, la marca puede estar en cientos de MiB, y "store full (310
		// MiB free)" sin más confundía.
		if len(pausadas) > 0 || (!v.lleno && libre < marcaPausaAlmacen) {
			msg := (&errAlmacenLleno{libre: libre}).Error()
			if len(pausadas) > 0 {
				log.Printf("%s: paused %d instance(s) writing to it (%d MiB/s) so their guests don't get I/O errors (%s); they resume on their own once there is room",
					msg, len(pausadas), int64(v.tasa)>>20, strings.Join(pausadas, ", "))
			} else {
				log.Printf("%s", msg)
			}
		}
		v.lleno = true
	case libre >= max(libreMinimaAlmacen, 2*marca):
		if v.sitio.IsZero() {
			v.sitio = v.t
		}
		// Siempre tras la espera, también la primera vez tras arrancar el
		// daemon: con las retenidas de un daemon anterior, reanudar en la
		// primera vuelta las devolvía a llenar el almacén.
		if v.t.Sub(v.sitio) >= esperaReanudar {
			m.reanudarPorAlmacen(ctx)
			v.lleno = false
		}
		return true
	}
	v.sitio = time.Time{}
	return true
}

// contiene dice si el overlay de la máquina id vive en el almacén: su
// machines/<id>/overlay.ext4 es un enlace que apunta dentro de a.dir.
func (a *almacenCoW) contiene(id string) bool {
	dest, err := os.Readlink(filepath.Join(a.root, "machines", id, "overlay.ext4"))
	return err == nil && strings.HasPrefix(filepath.Clean(dest), a.dir+string(filepath.Separator))
}

// comprobarAlmacenPara dice si la máquina id puede despertarse: si su overlay
// vive en el almacén y no queda libreMinimaAlmacen asignable, un
// *errAlmacenLleno. Fuera del almacén, o sin poder medirlo, nil.
func (m *Manager) comprobarAlmacenPara(id string) error {
	if m.alm == nil || !m.alm.estaListo() || !m.alm.contiene(id) {
		return nil
	}
	_, libre, err := m.alm.libreDentro()
	if err != nil || libre >= libreMinimaAlmacen {
		return nil
	}
	return &errAlmacenLleno{libre: libre}
}

// pausarPorAlmacen pausa (o, si no se puede pausar, congela) las máquinas en
// marcha cuyo overlay vive en el almacén, y devuelve sus nombres.
func (m *Manager) pausarPorAlmacen(ctx context.Context) []string {
	m.mu.RLock()
	var ids []string
	for id, mc := range m.byID {
		// La transición en curso vive en m.transicion: Transition solo se
		// rellena en las copias que se devuelven (Get, List), nunca en byID.
		// Mirar mc.Transition aquí no saltaba nunca a la que se congela.
		if mc.State == api.StateRunning && m.transicion[id] == "" {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()
	// Todas a la vez: mientras se pausa una, las demás siguen escribiendo.
	var (
		mu     sync.Mutex
		hechas []string
		wg     sync.WaitGroup
	)
	for _, id := range ids {
		if !m.alm.contiene(id) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if name, ok := m.pausarUnaPorAlmacen(ctx, id); ok {
				mu.Lock()
				hechas = append(hechas, name)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	sort.Strings(hechas)
	return hechas
}

// pausarUnaPorAlmacen pausa (o congela) la máquina id y le pone el Hold.
func (m *Manager) pausarUnaPorAlmacen(ctx context.Context, id string) (string, bool) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	mc, err := m.Pause(cctx, id)
	if err != nil {
		// Con carpetas vivas no se pausa (pausa.go): se congela. La memoria
		// va a la raíz, no al almacén. Sin los 5 s: volcar varios GiB tarda
		// más, y cortarlo a medias deja la máquina a medio congelar; el plazo
		// del volcado lo pone Freeze (plazoVolcado).
		mc, err = m.Freeze(context.WithoutCancel(ctx), id)
	}
	if err != nil {
		log.Printf("warning: copy-on-write store full: couldn't pause %s: %v", shortID(id), err)
		return "", false
	}
	m.mu.Lock()
	if live := m.byID[id]; live != nil {
		live.Hold = api.HoldStoreFull
		m.persist()
	}
	m.mu.Unlock()
	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvFrozen, ID: id, Name: mc.Name,
		Message: "on hold: " + api.HoldStoreFull})
	return mc.Name, true
}

// reanudarPorAlmacen reanuda las pausadas por el almacén lleno. Las que se
// congelaron (no se podían pausar) se quedan congeladas: el siguiente uso las
// despierta, como a cualquier otra.
func (m *Manager) reanudarPorAlmacen(ctx context.Context) {
	m.mu.RLock()
	var ids []string
	for id, mc := range m.byID {
		if mc.Hold == api.HoldStoreFull {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()
	for _, id := range ids {
		mc, ok := m.Get(id)
		if !ok {
			continue
		}
		if mc.State != api.StatePaused {
			m.quitarHold(id)
			continue
		}
		// Sin plazo propio, como Freeze arriba: un thaw cortado a mitad deja
		// la máquina a medio despertar.
		_, err := m.Thaw(context.WithoutCancel(ctx), id)
		var lleno *errAlmacenLleno
		switch {
		case errors.As(err, &lleno):
			return // otra vez sin sitio: la siguiente vuelta
		case err != nil:
			log.Printf("warning: copy-on-write store has room again, but %s couldn't be resumed: %v", mc.Name, err)
		default:
			log.Printf("copy-on-write store has room again: resumed %s", mc.Name)
		}
	}
}

// quitarHold borra el Hold de la máquina id.
func (m *Manager) quitarHold(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if live := m.byID[id]; live != nil && live.Hold != "" {
		live.Hold = ""
		m.persist()
	}
}

// vigiaOverlaysCada es cada cuánto mira vigilarOverlays los overlays. Un
// stat por máquina: nada. Entre dos vueltas un VMM que escribe a lo que da
// el disco gana unos GiB, que es lo más que se le escapa.
const vigiaOverlaysCada = time.Second

// vigilarOverlays es el vigilante de los overlays sin almacén (ver arriba).
func (m *Manager) vigilarOverlays(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(vigiaOverlaysCada):
			m.vueltaVigiaOverlays()
		}
	}
}

// vueltaVigiaOverlays es una vuelta de vigilarOverlays: para las máquinas en
// marcha (o pausadas: su VMM sigue vivo) cuyo overlay, en tamaño o en bloques,
// pasa de su tope (topeOverlayDe), y devuelve sus nombres.
//
// Parar y no pausar, como hace el almacén lleno: pausar se lo pide al propio
// VMM, que es justo quien no se porta bien; parar mata el proceso y deja el
// disco como está (lo de más allá del tamaño lógico el invitado no lo ve).
func (m *Manager) vueltaVigiaOverlays() []string {
	type vigilada struct {
		id, name string
		desde    time.Time
		pid      int
		tope     int64
	}
	m.mu.RLock()
	var vs []vigilada
	for id, mc := range m.byID {
		if (mc.State != api.StateRunning && mc.State != api.StatePaused) || m.transicion[id] != "" {
			continue
		}
		v := vigilada{id: id, name: mc.Name, pid: mc.PID, tope: topeOverlayDe(mc)}
		if mc.StartedAt != nil {
			v.desde = *mc.StartedAt
		}
		vs = append(vs, v)
	}
	m.mu.RUnlock()

	var paradas []string
	for _, v := range vs {
		ruta := filepath.Join(m.root, "machines", v.id, "overlay.ext4")
		fi, err := os.Stat(ruta)
		if err != nil {
			continue
		}
		mide := max(fi.Size(), allocatedBytes(ruta))
		if v.tope == 0 {
			// Primera vez que se la ve: se apunta su tope y ya no se vuelve
			// a medir (ni al reanudarla, ni al descongelarla, ni con otro
			// daemon).
			v.tope = m.apuntarTopeOverlay(v.id, v.name, mide)
		}
		if v.tope == 0 || mide <= v.tope {
			continue
		}
		log.Printf("warning: %s: its disk file grew to %d MiB, past the %d MiB its guest can use; stopping it so it can't fill the host disk (its VMM may be compromised)",
			v.name, mide>>20, v.tope>>20)
		// Solo si sigue en la ejecución que se midió: si entre medias la
		// pararon y la arrancaron otra vez, esta vuelta no la para.
		misma := func(mc *api.Machine) bool {
			return (mc.State == api.StateRunning || mc.State == api.StatePaused) &&
				mc.PID == v.pid && mc.StartedAt != nil && mc.StartedAt.Equal(v.desde)
		}
		if _, err := m.stopSi(v.id, misma); err != nil {
			if !errors.Is(err, errOtraEjecucion) {
				log.Printf("warning: %s: couldn't stop it: %v", v.name, err)
			}
			continue
		}
		paradas = append(paradas, v.name)
	}
	sort.Strings(paradas)
	return paradas
}

// topeOverlayDe es lo que puede ocupar el overlay de mc: lo que su invitado
// puede direccionar (DiskMiB, o el de siempre) más el margen de cuotaInstancia,
// o el tope que se le apuntó si es mayor. 0 si aún no se le ha apuntado
// ninguno (lo apunta la primera vuelta que la ve).
func topeOverlayDe(mc *api.Machine) int64 {
	if mc.OverlayMaxBytes <= 0 {
		return 0
	}
	return max(mc.OverlayMaxBytes, cuotaInstancia(logicoOverlay(mc)))
}

// logicoOverlay es el tamaño lógico del disco escribible de mc.
func logicoOverlay(mc *api.Machine) int64 {
	mib := mc.DiskMiB
	if mib <= 0 {
		mib = defaultOverlayMiB
	}
	return int64(mib) << 20
}

// apuntarTopeOverlay fija y persiste el tope del overlay de id, que mide mide
// la primera vez que el vigilante lo ve. Si ya es mayor que su disco (un disco
// que vino así de un snapshot, o lo que creciera con un daemon anterior), el
// tope sale de lo que mide: no se para lo que ya estaba, pero tampoco crece
// más. Como queda apuntado, eso pasa una sola vez: lo que el invitado alargue
// el fichero antes de un vistazo posterior (tras un thaw, una reanudación o un
// reinicio del daemon) ya no le sube el tope. Devuelve el tope (0 si la
// máquina ya no está).
func (m *Manager) apuntarTopeOverlay(id, name string, mide int64) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	mc := m.byID[id]
	if mc == nil {
		return 0
	}
	if mc.OverlayMaxBytes > 0 {
		return topeOverlayDe(mc) // lo apuntó otra vuelta entre medias
	}
	logico := logicoOverlay(mc)
	tope := cuotaInstancia(logico)
	if mide > tope {
		log.Printf("%s: its disk file is already %d MiB, more than its %d MiB disk; it may not grow past %d MiB",
			name, mide>>20, logico>>20, cuotaInstancia(mide)>>20)
		tope = cuotaInstancia(mide)
	}
	mc.OverlayMaxBytes = tope
	m.persist()
	return tope
}
