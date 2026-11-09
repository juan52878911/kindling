package machine

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// PAUSAR ANTES DE DORMIR (macOS).
//
// despertar_host.go arregla el reloj de las máquinas DESPUÉS de que el host
// durmiera, porque sin cgo el núcleo no se entera antes. Pero en macOS sí se
// puede saber: el binario del VMM (cgo) tiene un modo vigía (`kling-vz
// --power-events`, vz/internal/energia) que se registra con
// IORegisterForSystemPower y avisa por stdout de que el Mac se va a dormir;
// macOS espera su respuesta (hasta ~30 s; el vigía no espera más de 10). El
// daemon pausa entonces las máquinas que corren, contesta, y al despertar las
// reanuda (Thaw; como la pausa es de antes del sueño, reanudarLocked además
// resincroniza su reloj). Así un invitado no se
// queda congelado a mitad de lo que hacía cuando el Mac se para por debajo,
// sino en una pausa limpia del VMM. Si el vigía no arranca (un VMM anterior
// sin el modo, Linux), queda lo de siempre: resincronizar al despertar.

// LA FUENTE DE ALIMENTACIÓN. El vigía también dice de dónde come el Mac
// ("power battery" / "power ac") al arrancar y en cada cambio
// (IOPSCreateLimitedPowerNotification). Mientras viva, el modo batería en auto
// se cree eso y no lanza pmset cada minuto (enBateriaSO); si muere, o es un
// vigía anterior que no lo dice, vuelve a preguntar a pmset.

// fuenteVigia es lo último que dijo el vigía de la fuente: fuenteNoSe,
// fuenteCorriente o fuenteBateria. Del proceso y no del Manager porque
// enBateriaSO no tiene Manager; el daemon tiene uno solo.
var fuenteVigia atomic.Int32

const (
	fuenteNoSe = iota
	fuenteCorriente
	fuenteBateria
)

// plazoPausaSueno es lo que se dedica a pausar antes de contestar al vigía,
// que deja dormir al Mac a los 10 s con o sin respuesta.
const plazoPausaSueno = 7 * time.Second

// atenderEnergia habla con el vigía: lee sus avisos de r y contesta por w
// hasta que r se acaba. Reanuda lo que pausó si el vigía muere entre el
// sueño y el despertar.
func (m *Manager) atenderEnergia(ctx context.Context, r io.Reader, w io.Writer) {
	var pausadas []string
	defer func() { m.reanudarTrasSueno(ctx, pausadas) }()
	// Sin vigía no hay quien avise del cambio: que se vuelva a preguntar.
	defer fuenteVigia.Store(fuenteNoSe)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		linea := sc.Text()
		switch {
		case strings.HasPrefix(linea, "sleep "):
			id := strings.TrimPrefix(linea, "sleep ")
			pausadas = append(pausadas, m.pausarParaDormir(ctx)...)
			if _, err := fmt.Fprintf(w, "ack %s\n", id); err != nil {
				return
			}
		case linea == "power battery" || linea == "power ac":
			m.anotarFuente(linea == "power battery")
		case linea == "wake":
			m.reanudarTrasSueno(ctx, pausadas)
			pausadas = nil
			m.revisarSueno(ctx)
		}
	}
}

// anotarFuente apunta lo que dijo el vigía de la fuente y lo pone ya en la
// lectura cacheada del modo batería: el cambio vale desde ahora, no desde que
// caduque la lectura anterior.
func (m *Manager) anotarFuente(bat bool) {
	v := int32(fuenteCorriente)
	if bat {
		v = fuenteBateria
	}
	if fuenteVigia.Swap(v) != v {
		donde := "AC power"
		if bat {
			donde = "battery"
		}
		log.Printf("power: running on %s", donde)
	}
	m.bat.mu.Lock()
	m.bat.enBat, m.bat.leidaT = bat, m.bat.now()
	m.bat.mu.Unlock()
}

// pausarParaDormir pausa las máquinas que corren y devuelve cuáles pausó.
func (m *Manager) pausarParaDormir(ctx context.Context) []string {
	var ids []string
	for _, mc := range m.List() {
		if mc.State == api.StateRunning && mc.PID > 0 {
			ids = append(ids, mc.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, plazoPausaSueno)
	defer cancel()
	t0 := time.Now()
	var mu sync.Mutex
	var hechas []string
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.pausarBat(ctx, id); err != nil {
				log.Printf("host sleep: pausing %s: %v", shortID(id), err)
				return
			}
			mu.Lock()
			hechas = append(hechas, id)
			mu.Unlock()
		}()
	}
	wg.Wait()
	log.Printf("the host is going to sleep: paused %d of %d running machine(s) in %d ms",
		len(hechas), len(ids), time.Since(t0).Milliseconds())
	return hechas
}

// reanudarTrasSueno reanuda las que pausó pausarParaDormir y siguen en pausa
// (otro pudo reanudarlas o borrarlas mientras tanto).
func (m *Manager) reanudarTrasSueno(ctx context.Context, ids []string) {
	if len(ids) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		mc, ok := m.Get(id)
		if !ok || mc.State != api.StatePaused {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.reanudarBat(ctx, id); err != nil {
				log.Printf("host woke: resuming %s: %v", mc.Name, err)
			}
		}()
	}
	wg.Wait()
	log.Printf("the host woke: resumed the machine(s) paused before sleeping (%d)", len(ids))
}
