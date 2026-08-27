package machine

// Recolección de espacio a largo plazo.
//
// Cada máquina warm conserva su mem.file, del tamaño de su RAM. Eso es correcto
// —es lo que permite descongelar en milisegundos—, pero significa que N
// servicios dormidos son N gigabytes en disco, sin que nada crezca "mal": es el
// coste normal acumulándose. En un anfitrión con disco holgado da igual; en uno
// justo, llena el disco y entonces NADA arranca, porque una microVM reserva su
// memoria por adelantado.
//
// La política: cuando el disco pasa de una marca alta, se ELIMINAN las
// instancias warm que se pueden recrear —las que vienen de un snapshot dorado
// que sigue existiendo—, de más antigua a más nueva, hasta bajar de una marca
// objetivo. Eliminar no es perder: su estado vive en el snapshot, y volver
// cuesta ~200 ms. Lo que NO se toca es una warm sin snapshot de respaldo: ahí el
// mem.file ES el único estado, y borrarlo sería perder datos.

import (
	"context"
	"log"
	"os"
	"sort"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

const (
	// diskHighPct: por encima, se empieza a recuperar espacio.
	diskHighPct = 90
	// diskTargetPct: se recupera hasta bajar aquí. El hueco entre las dos evita
	// expulsar en cada tick por un fichero temporal que sube y baja.
	diskTargetPct = 80
)

// diskUsedPct devuelve el porcentaje de disco usado en la raíz de datos, o -1 si
// no se puede saber (en cuyo caso quien llama no debe hacer nada: mejor no
// recolectar que recolectar a ciegas).
func (m *Manager) diskUsedPct() int {
	var st syscall.Statfs_t
	if err := syscall.Statfs(m.root, &st); err != nil {
		return -1
	}
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bavail * uint64(st.Bsize)
	if total == 0 {
		return -1
	}
	return int(100 * (total - free) / total)
}

// gcDisk libera disco eliminando instancias warm recuperables si se pasa de la
// marca alta.
//
// Recuperable = viene de un snapshot dorado (From != "") que todavía existe. Se
// eliminan de más antigua a más nueva: la que lleva más tiempo dormida es la que
// menos probable es que se pida pronto.
func (m *Manager) gcDisk(ctx context.Context) {
	pct := m.diskUsedPct()
	if pct < diskHighPct {
		return
	}

	type cand struct {
		id    string
		name  string
		since int64
	}
	m.mu.RLock()
	var cands []cand
	for _, mc := range m.byID {
		if mc.State != api.StateWarm || mc.From == "" {
			continue
		}
		// Solo si su snapshot sigue ahí para recrearla. Sin él, esta warm es
		// irrecuperable y no se toca.
		if _, err := m.loadSnapshot(mc.From); err != nil {
			continue
		}
		var since int64
		if mc.FrozenAt != nil {
			since = mc.FrozenAt.UnixNano()
		}
		cands = append(cands, cand{mc.ID, mc.Name, since})
	}
	m.mu.RUnlock()

	// Más antigua (FrozenAt menor) primero.
	sort.Slice(cands, func(i, j int) bool { return cands[i].since < cands[j].since })

	for _, c := range cands {
		if m.diskUsedPct() < diskTargetPct {
			return
		}
		if err := m.Remove(c.id); err != nil {
			log.Printf("gc: couldn't remove dormant instance %s: %v", c.name, err)
			continue
		}
		log.Printf("gc: disk at %d%%, removed dormant instance %s (recreates from %s)",
			m.diskUsedPct(), c.name, c.name)
	}

	if p := m.diskUsedPct(); p >= diskHighPct {
		// Se hizo lo que se pudo y sigue lleno: hay que decirlo, porque el
		// siguiente síntoma será un arranque que falla por disco y no por esto.
		log.Printf("gc: disk still at %d%% after recovering what could be recovered; "+
			"check images, volumes, or instances without a backup snapshot", p)
	}
}

// defaultFailedRetention es cuánto se conserva una máquina failed antes de
// recogerla. Una hora: de sobra para que `kling ps -a` y su LastErr cuenten qué
// pasó, y lo bastante corto para que los intentos fallidos no se acumulen — se
// observaron nueve en 21 horas, uno por instanciación rota, para siempre.
const defaultFailedRetention = time.Hour

// failedRetention devuelve la retención, ajustable con KLING_FAILED_RETENTION
// (una duración de Go: "30m", "2h"; "0" desactiva la recogida).
func failedRetention() time.Duration {
	if v := os.Getenv("KLING_FAILED_RETENTION"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
	}
	return defaultFailedRetention
}

// gcFailed recoge las máquinas failed que ya cumplieron su tiempo de gracia.
//
// Automático y no un comando, por la misma razón que gcDisk: failed es un
// estado TERMINAL (no hay `start` que lo saque de ahí), así que conservarlas no
// da ninguna opción nueva — solo diagnóstico, y para eso basta la ventana de
// gracia. Un daemon que exige limpieza manual de sus propios restos acumula
// restos, que es exactamente lo observado.
//
// Una failed sin fecha (estado anterior al campo FailedAt) no se borra al
// instante: se le arranca el reloj ahora y cae en la pasada que le toque. Es la
// diferencia entre recoger basura y borrar algo que quizá falló hace un minuto.
func (m *Manager) gcFailed() {
	retention := failedRetention()
	if retention <= 0 {
		return
	}
	now := time.Now()

	type victim struct{ id, name, lastErr string }
	var due []victim
	stamped := false

	m.mu.Lock()
	for _, mc := range m.byID {
		if mc.State != api.StateFailed {
			continue
		}
		if mc.FailedAt == nil {
			t := now
			mc.FailedAt = &t
			stamped = true
			continue
		}
		if now.Sub(*mc.FailedAt) >= retention {
			due = append(due, victim{mc.ID, mc.Name, mc.LastErr})
		}
	}
	if stamped {
		m.persist()
	}
	m.mu.Unlock()

	// Fuera del candado: Remove toma el suyo y además toca disco y red.
	for _, v := range due {
		if err := m.Remove(v.id); err != nil {
			log.Printf("gc: couldn't collect failed machine %s: %v", v.name, err)
			continue
		}
		log.Printf("gc: collected failed machine %s (failed with: %s)", v.name, v.lastErr)
	}
}
