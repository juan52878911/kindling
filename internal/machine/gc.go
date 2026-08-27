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
	"strconv"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

const (
	// defaultDiskHighPct: por encima, se empieza a recuperar espacio.
	defaultDiskHighPct = 90
	// defaultDiskTargetPct: se recupera hasta bajar aquí. El hueco entre las dos
	// evita expulsar en cada tick por un fichero temporal que sube y baja.
	defaultDiskTargetPct = 80
)

// gcDiskHighPct es el umbral por encima del cual empieza la recuperación de
// disco. Ajustable con KLING_GC_DISK_HIGH (1-100), en la línea del resto de
// knobs del daemon (KLING_MAX_PARALLEL_BOOT, KLING_MIN_FREE_MIB…). Un valor
// fuera de rango se ignora y se usa el defecto.
func gcDiskHighPct() int {
	if v := os.Getenv("KLING_GC_DISK_HIGH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 100 {
			return n
		}
	}
	return defaultDiskHighPct
}

// gcDiskTargetPct es hasta dónde se recupera. Ajustable con KLING_GC_DISK_TARGET.
// Debe quedar por DEBAJO de la marca alta: el hueco es lo que evita expulsar en
// cada tick. Si el valor dado no respeta esa invariante, se acota a high-1, que
// es lo mínimo que sigue teniendo sentido.
func gcDiskTargetPct(high int) int {
	target := defaultDiskTargetPct
	if v := os.Getenv("KLING_GC_DISK_TARGET"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			target = n
		}
	}
	if target >= high {
		target = high - 1
	}
	return target
}

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
// gcPausaInutil es cuanto se deja de expulsar cuando expulsar no sirve.
const gcPausaInutil = 30 * time.Minute

// evictarSirve dice si merece la pena seguir expulsando.
//
// La marca alta se mide sobre TODO el sistema de ficheros, no sobre la huella de
// kindling. Si lo que llena el disco es de otro —observado en el laboratorio:
// containerd con 7,8 GB de 20— expulsar no lo va a arreglar, y lo unico que
// consigue es destruir el warm-pooling, que es el valor entero del producto:
// cada instancia se expulsaba entre 1 y 11 segundos despues de congelarse, cada
// diez segundos, indefinidamente. El daemon lo decia en el log una vez por tick
// durante un dia entero y todas las llamadas pagaban un arranque en frio de
// 30-40 s sin que nada correlacionara las dos cosas.
//
// Sirve si bajo del umbral, o si al menos hizo progreso: parar tras una pasada
// que SI libero seria dejar el disco llenandose.
func evictarSirve(antes, despues, alta int) bool {
	return despues < alta || despues < antes
}

func (m *Manager) gcDisk(ctx context.Context) {
	// En pausa: la pasada anterior no consiguio nada y el disco no es nuestro.
	if !m.gcPausadoHasta.IsZero() && time.Now().Before(m.gcPausadoHasta) {
		return
	}
	high := gcDiskHighPct()
	target := gcDiskTargetPct(high)
	pct := m.diskUsedPct()
	if pct < high {
		return
	}
	antes := pct

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
		if m.diskUsedPct() < target {
			return
		}
		if err := m.Remove(c.id); err != nil {
			log.Printf("gc: couldn't remove dormant instance %s: %v", c.name, err)
			continue
		}
		log.Printf("gc: disk at %d%%, removed dormant instance %s (recreates from %s)",
			m.diskUsedPct(), c.name, c.name)
	}

	if p := m.diskUsedPct(); p >= high {
		if evictarSirve(antes, p, high) {
			// Bajo algo pero no lo suficiente: se sigue en el proximo tick.
			log.Printf("gc: disk still at %d%% after recovering what could be recovered; "+
				"check images, volumes, or instances without a backup snapshot", p)
			return
		}
		// No bajo NADA. Lo que ocupa el disco no es de kindling, asi que seguir
		// expulsando solo cuesta warm-pooling. Se para un rato y se dice claro.
		m.gcPausadoHasta = time.Now().Add(gcPausaInutil)
		log.Printf("gc: evicting freed nothing and the disk is still at %d%% — what fills it "+
			"is NOT kindling's. Eviction paused for %s so warm instances stop being thrown "+
			"away for nothing; look at what else uses this filesystem (du -sh /var/lib/*).",
			p, gcPausaInutil)
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
