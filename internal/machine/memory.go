package machine

// No arrancar microVMs que no caben en el anfitrión.
//
// Una microVM pide su memoria por adelantado y el invitado la usa de verdad: no
// hay sobrecompromiso que valga cuando dentro corre un servidor que se la come.
// Arrancar la cuarta de 1024 MiB en un host que solo tiene 1,8 GiB no da un
// error de arranque — arranca, y después el OOM killer del ANFITRIÓN empieza a
// matar procesos al azar, incluidos otros firecracker y el propio daemon.
//
// El síntoma es de los peores del proyecto: microVMs que mueren solas, sin
// motivo en su consola, mientras el fallo real está en un journal del host que
// nadie mira. Comprobado en el laboratorio: nueve líneas de OOM del kernel y
// treinta de cuarenta llamadas fallando.

import (
	"fmt"
	"sync"

	"github.com/juan52878911/kindling/internal/api"
	"os"
	"strconv"
	"strings"
)

// hostReserveMiB es lo que se le deja al anfitrión pase lo que pase: el daemon,
// el gateway, sshd y el margen del propio kernel. Sin esta reserva, la última
// microVM que cabe deja al host sin aire y el OOM killer elige a quien quiera.
const hostReserveMiB = 384

// availableMiB devuelve la memoria realmente disponible del anfitrión.
//
// MemAvailable y no MemFree: incluye la caché reclamable, que es lo que de
// verdad se puede usar. MemFree solo mediría lo que está sin tocar, y en un host
// que lleva horas sirviendo imágenes eso es casi nada aunque haya gigas
// reclamables.
//
// Devuelve 0 si no se puede saber; quien llama debe tratarlo como "no comprobar"
// en vez de como "no hay memoria".
func availableMiB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(line, "MemAvailable:")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			return 0
		}
		kb, err := strconv.Atoi(f[0])
		if err != nil {
			return 0
		}
		return kb / 1024
	}
	return 0
}

// checkHostMemory se niega a arrancar una microVM que no cabe.
//
// Es un aviso caro de ignorar y barato de dar: leer /proc/meminfo cuesta
// microsegundos, y el fallo que evita cuesta una tarde de diagnóstico.
//
// No comprueba nada si no puede leer la memoria: convertir una comprobación de
// cordura en una dependencia de arranque sería peor que el problema.
// reserveMemory comprueba que la microVM cabe y, si cabe, RESERVA su memoria
// hasta que termine de arrancar. Devuelve una función para liberar la reserva,
// que hay que llamar SIEMPRE —arranque bien o mal— o la reserva se queda colgada
// y bloquea a los siguientes.
//
// La reserva es lo que cierra el TOCTOU: entre leer la memoria y que el invitado
// la ocupe de verdad pasan segundos, y en ese hueco otro arranque veía la misma
// memoria libre. Contar lo que ya está en vuelo hace que el segundo vea que no
// cabe y espere su turno —o desaloje— en vez de sumarse al desbordamiento.
func (m *Manager) reserveMemory(wantMiB int) (release func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := checkHostMemory(wantMiB + m.pendingMiB); err != nil {
		return nil, err
	}
	m.pendingMiB += wantMiB
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			m.pendingMiB -= wantMiB
			if m.pendingMiB < 0 {
				m.pendingMiB = 0
			}
			m.mu.Unlock()
		})
	}, nil
}

func checkHostMemory(wantMiB int) error {
	avail := availableMiB()
	if avail <= 0 || wantMiB <= 0 {
		return nil
	}
	if wantMiB+hostReserveMiB <= avail {
		return nil
	}
	return &api.StatusError{Code: api.StatusInsufficientMemory, Message: fmt.Sprintf(
		"no cabe: la microVM pide %d MiB y al anfitrión le quedan %d MiB "+
			"(se reservan %d para el propio host).\n"+
			"Arrancarla igualmente haría que el OOM killer del anfitrión matara procesos al azar, "+
			"incluidas otras microVMs.\nPara: `kling ps -a`, o dale menos memoria con -mem",
		wantMiB, avail, hostReserveMiB)}
}
