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
	"sort"
	"syscall"

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
			log.Printf("gc: no pude eliminar la instancia dormida %s: %v", c.name, err)
			continue
		}
		log.Printf("gc: disco al %d%%, eliminé la instancia dormida %s (se recrea desde %s)",
			m.diskUsedPct(), c.name, c.name)
	}

	if p := m.diskUsedPct(); p >= diskHighPct {
		// Se hizo lo que se pudo y sigue lleno: hay que decirlo, porque el
		// siguiente síntoma será un arranque que falla por disco y no por esto.
		log.Printf("gc: disco todavía al %d%% tras recuperar lo recuperable; "+
			"revisa imágenes, volúmenes o instancias sin snapshot de respaldo", p)
	}
}
