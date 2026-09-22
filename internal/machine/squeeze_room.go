package machine

// Hacer sitio antes de rendirse.
//
// Una microVM ociosa retiene RAM que su invitado ya no usa: páginas libres y
// caché limpia que el globo puede devolver al host SIN congelar nada y sin que
// el invitado se entere (ver Squeeze). Hasta ahora eso solo pasaba cuando
// alguien tecleaba `kling squeeze`, así que el daemon rechazaba arranques con
// 507 teniendo cientos de MiB recuperables a un ioctl de distancia.
//
// El orden importa: primero se pide prestado lo que sobra (barato, reversible,
// invisible), y solo después se congela o se desaloja a alguien (caro, visible).
// Este fichero es el primer escalón; el segundo vive en el planificador.

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// machineLimitMarkText es la marca que api.IsMachineLimit reconoce. Vive aquí
// como texto y allí como constante: el API no debe importar el manager.
const machineLimitMarkText = "machine limit"

// squeezeCooldown evita que una ráfaga de arranques apriete el mismo globo una
// vez por arranque: reclamar lo mismo dos veces seguidas no devuelve nada nuevo
// y cuesta una pausa en el invitado.
const squeezeCooldown = 30 * time.Second

// squeezeBudget acota cuánto se tarda en hacer sitio. Quien espera es alguien
// que está arrancando una microVM; si esto se alarga, es peor que el 507.
const squeezeBudget = 5 * time.Second

// makeRoom pide a los invitados vivos que devuelvan la memoria que no usan, y
// devuelve los MiB recuperados. Es best-effort: un invitado sin globo (imagen
// anterior), pausado o que no contesta se salta sin ruido.
//
// skip es la máquina que está naciendo, si ya está registrada.
func (m *Manager) makeRoom(ctx context.Context, skip string) int {
	ctx, cancel := context.WithTimeout(ctx, squeezeBudget)
	defer cancel()

	type cand struct {
		id   string
		mem  int
		used time.Time
	}
	var cands []cand
	ahora := time.Now()

	m.mu.Lock()
	if m.squeezedAt == nil {
		m.squeezedAt = map[string]time.Time{}
	}
	for id, mc := range m.byID {
		if id == skip || mc.State != api.StateRunning || mc.PID == 0 {
			continue
		}
		if ahora.Sub(m.squeezedAt[id]) < squeezeCooldown {
			continue
		}
		desde := time.Time{}
		if mc.StartedAt != nil {
			desde = *mc.StartedAt
		}
		cands = append(cands, cand{id: id, mem: mc.MemMiB, used: desde})
	}
	m.mu.Unlock()

	// Las más grandes primero: devuelven más por apretón, y el objetivo es
	// llegar al umbral cuanto antes.
	sort.Slice(cands, func(i, j int) bool { return cands[i].mem > cands[j].mem })

	total := 0
	for _, c := range cands {
		if ctx.Err() != nil {
			break
		}
		// Sin esperar: una máquina ocupada (congelándose, arrancando) se salta.
		// Bloquear aquí pondría el arranque que pide sitio a esperar detrás de
		// ella, que es exactamente lo que se quiere evitar.
		soltar, ok := m.tryLock(c.id)
		if !ok {
			continue
		}
		res, err := m.squeezeLocked(ctx, c.id, c.id)
		soltar()
		m.mu.Lock()
		m.squeezedAt[c.id] = time.Now()
		m.mu.Unlock()
		if err != nil {
			continue
		}
		total += res.ReclaimedMiB
	}
	if total > 0 {
		log.Printf("memory: reclaimed %d MiB from %d live microVM(s) with the balloon before giving up", total, len(cands))
	}
	return total
}

// reserveMemoryMakingRoom reserva memoria y, si no cabe, pide prestado a los
// globos de los invitados vivos y lo intenta una vez más.
//
// Un solo reintento a propósito: si tras devolver lo devolvible sigue sin caber,
// insistir solo retrasa un 507 que va a llegar igual, y quien espera está
// arrancando una microVM.
func (m *Manager) reserveMemoryMakingRoom(ctx context.Context, wantMiB int, shareKey, skip string) (func(), error) {
	release, err := m.reserveMemory(wantMiB, shareKey)
	if err == nil || !api.IsInsufficientMemory(err) {
		return release, err
	}
	if m.makeRoom(ctx, skip) <= 0 {
		return nil, err
	}
	return m.reserveMemory(wantMiB, shareKey)
}

// maxMachines es el tope de máquinas por daemon. Ver defaultMaxMachines: es un
// límite de seguridad, no de capacidad, y con sandboxes efímeros en un host
// grande se queda corto. KLING_MAX_MACHINES lo sube; 0 o basura = el de siempre.
func maxMachines() int {
	if v := os.Getenv("KLING_MAX_MACHINES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxMachines
}

// checkMachineLimit rechaza cuando ya no caben más máquinas, con un error que se
// puede DISTINGUIR de la falta de memoria.
//
// Antes era un error genérico, y eso tenía una consecuencia concreta: el
// planificador, al ver que no era un 507, ni desalojaba ni reintentaba, y quien
// llamaba recibía "limit of 256 machines reached" sin saber que la mayoría eran
// máquinas paradas o fallidas que se recogen solas. El mensaje ahora dice de qué
// están hechas esas 256.
func (m *Manager) checkMachineLimit() error {
	tope := maxMachines()

	m.mu.RLock()
	n := len(m.byID)
	porEstado := map[api.State]int{}
	for _, mc := range m.byID {
		porEstado[mc.State]++
	}
	m.mu.RUnlock()

	if n < tope {
		return nil
	}
	// Recoger cadáveres antes de rendirse: una failed vieja ocupa un hueco y se
	// iba a recoger sola en la siguiente vuelta del vigilante de todos modos.
	if porEstado[api.StateFailed] > 0 || porEstado[api.StateStopped] > 0 {
		m.gcFailed()
		m.mu.RLock()
		n = len(m.byID)
		m.mu.RUnlock()
		if n < tope {
			return nil
		}
	}
	return &api.StatusError{Code: api.StatusMachineLimit, Message: fmt.Sprintf(
		"%s reached: %d of %d machines (%d running, %d warm, %d failed, %d stopped).\n"+
			"Remove what you don't need (`kling ps -a`, `kling rm`), or raise the cap with KLING_MAX_MACHINES on the daemon",
		machineLimitMarkText, n, tope,
		porEstado[api.StateRunning], porEstado[api.StateWarm],
		porEstado[api.StateFailed], porEstado[api.StateStopped])}
}
