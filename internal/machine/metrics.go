package machine

import (
	"bufio"
	"bytes"
	"os"
	"strconv"

	"github.com/juan52878911/kindling/internal/api"
)

// ProcStats toma una foto del consumo de memoria del conjunto de microVMs.
//
// La lectura de /proc se hace FUERA del lock: recorrer smaps_rollup de cada
// proceso es E/S, y sostener m.mu mientras tanto bloquearía arranques y
// congelaciones. Bajo el lock solo se copia la lista; el PSS se resuelve después
// contra el PID ya capturado.
func (m *Manager) ProcStats() api.ProcStats {
	m.mu.RLock()
	stats := make([]api.ProcStat, 0, len(m.byID))
	byState := make(map[string]int, 5)
	for _, mc := range m.byID {
		byState[string(mc.State)]++
		stats = append(stats, api.ProcStat{
			ID: mc.ID, Name: mc.Name, Service: mc.Service(),
			From: mc.From, State: mc.State, PID: mc.PID,
		})
	}
	m.mu.RUnlock()

	var total int64
	for i := range stats {
		// Solo las máquinas vivas tienen proceso; las warm/stopped no gastan RAM.
		if stats[i].PID > 0 {
			stats[i].PSSMiB = pssMiB(stats[i].PID)
			total += stats[i].PSSMiB
		}
	}
	avail, free := hostMemMiB()
	return api.ProcStats{
		Machines: stats, ByState: byState, TotalPSSMiB: total,
		AvailableMiB: avail, FreeMiB: free,
	}
}

// pssMiB devuelve el PSS del proceso en MiB leído de /proc/<pid>/smaps_rollup.
//
// PSS y no RSS: con el mem.file de un snapshot compartido por copy-on-write entre
// todas sus instancias, el RSS de cada proceso incluye el fichero entero y las
// copias mienten por un factor N. El PSS reparte cada página compartida entre
// quienes la mapean, así que sumar los PSS sí da lo que ocupa el host.
//
// smaps_rollup es el agregado que el kernel ya calcula: una sola línea "Pss:" en
// vez de recorrer todos los mapeos a mano. Si no existe (macOS de desarrollo, o
// un kernel sin la opción) se degrada a 0 sin romper.
func pssMiB(pid int) int64 {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/smaps_rollup")
	if err != nil {
		return 0
	}
	if kib := fieldKiB(b, "Pss:"); kib > 0 {
		return (kib + 512) >> 10 // kiB -> MiB, redondeando
	}
	return 0
}

// hostMemMiB lee MemAvailable y MemFree de /proc/meminfo, en MiB.
//
// MemAvailable es la estimación del kernel de cuánto se puede reservar sin entrar
// a swap; MemFree es lo que está literalmente sin usar. Ambas cifras en 0 cuando
// no hay /proc (dev sobre macOS).
func hostMemMiB() (available, free int64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	return fieldKiB(b, "MemAvailable:") >> 10, fieldKiB(b, "MemFree:") >> 10
}

// fieldKiB extrae el valor (en kiB) de la primera línea que empieza por prefix
// en un fichero estilo /proc, cuyo formato es "Etiqueta:   1234 kB".
func fieldKiB(data []byte, prefix string) int64 {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte(prefix)) {
			continue
		}
		f := bytes.Fields(line[len(prefix):])
		if len(f) == 0 {
			return 0
		}
		n, err := strconv.ParseInt(string(f[0]), 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}
