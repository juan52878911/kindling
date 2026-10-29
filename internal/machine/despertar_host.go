package machine

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// EL HOST DURMIÓ.
//
// Con el Mac dormido no corre nada, tampoco los invitados, y su reloj (el
// contador del temporizador de ARM, que en Apple Silicon no avanza dormido)
// se queda atrás lo que durmió: una máquina volvía con la hora de antes de
// cerrar la tapa, y con ella los TTL, los certificados y los plazos de
// dentro. Lo mismo en Linux con un portátil que se suspende.
//
// Saberlo no pide notificaciones del sistema (IORegisterForSystemPower
// necesitaría cgo, y el núcleo no lo usa): el reloj monotónico de Go no
// cuenta el tiempo dormido (mach_absolute_time, CLOCK_MONOTONIC) y el de
// pared sí, así que lo que el de pared adelanta al monotónico entre dos
// miradas es lo que durmió el host. Se mira en cada vuelta del vigilante y en
// cada petición contra una máquina (Usar), que es cuando importa: un exec
// justo al despertar ve ya la hora buena. Nada de eso añade despertares.
//
// En macOS, además, el vigía de energía (energia.go: el binario del VMM, que
// sí es cgo, en un proceso aparte) avisa ANTES de dormir y las máquinas se
// pausan; esto queda para Linux, para un VMM sin vigía y para las que no se
// pudieron pausar.

// suenoMin es lo menos que tiene que haber dormido el host para resincronizar:
// por debajo, lo que se desvían los dos relojes sin dormir.
const suenoMin = 2 * time.Second

// arranqueReloj es el origen del reloj monotónico de relojHost.
var arranqueReloj = time.Now()

// relojHost vigila los dos relojes.
type relojHost struct {
	mu    sync.Mutex
	pared time.Time     // sin lectura monotónica
	mono  time.Duration // desde arranqueReloj
	hay   bool
	// sueno es la hora de pared a la que se vio por última vez que el host
	// había dormido; cero = no se ha visto. Una pausada desde antes no ha
	// visto pasar ese tiempo (ver pausaAntesDelSueno).
	sueno time.Time
	// leer es un punto de inyección de las pruebas: la hora de pared y el
	// tiempo monotónico. nil = los de verdad.
	leer func() (time.Time, time.Duration)
}

// durmio dice cuánto durmió el host desde la última mirada (0 si nada) y
// rearma la base.
func (r *relojHost) durmio() time.Duration {
	leer := r.leer
	if leer == nil {
		leer = func() (time.Time, time.Duration) {
			return time.Now().Round(0), time.Since(arranqueReloj) // Round(0): solo pared
		}
	}
	pared, mono := leer()
	r.mu.Lock()
	defer r.mu.Unlock()
	d := pared.Sub(r.pared) - (mono - r.mono)
	hay := r.hay
	r.pared, r.mono, r.hay = pared, mono, true
	if hay && d >= suenoMin {
		r.sueno = pared
		return d
	}
	return 0
}

// pausaAntesDelSueno dice si una máquina pausada en desde lleva en pausa desde
// antes del último sueño del host que se vio (M8). El reloj del invitado
// (el temporizador de ARM en Apple Silicon) no avanza con el Mac dormido, así
// que reanudarla sin más la devolvía con la hora de antes de cerrar la tapa:
// revisarSueno solo resincroniza las que corren, y pausarParaDormir pausa
// justo las que corrían. Ante la duda (pausada entre la última mirada y la
// que vio el sueño) dice que sí: un resync de más cuesta milisegundos.
func (r *relojHost) pausaAntesDelSueno(desde *time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return desde != nil && !r.sueno.IsZero() && desde.Before(r.sueno)
}

// revisarSueno resincroniza el reloj de las máquinas que corren si el host
// durmió desde la última mirada. Las congeladas lo hacen al despertarlas
// (Thaw ya resincroniza) y las pausadas al reanudarlas si la pausa es anterior
// al sueño (reanudarLocked, pausaAntesDelSueno).
func (m *Manager) revisarSueno(ctx context.Context) {
	d := m.reloj.durmio()
	if d == 0 {
		return
	}
	var ids []string
	for _, mc := range m.List() {
		if mc.State == api.StateRunning && mc.Reachable() {
			ids = append(ids, mc.ID)
		}
	}
	log.Printf("the host slept %v: resyncing the clock of %d running machine(s)", d.Round(time.Second), len(ids))
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.resyncGuest(ctx, id, "", api.ResyncThaw)
		}()
	}
	wg.Wait()
}
