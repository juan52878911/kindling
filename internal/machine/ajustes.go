package machine

import (
	"os"
	"strconv"
)

// Ajustes devuelve los valores EFECTIVOS de los ajustes del daemon que se
// cambian por entorno (KLING_*), con el nombre de su variable como clave.
//
// Efectivos y no lo que pone el entorno: un valor fuera de rango se ignora y
// queda el defecto, y varios defectos dependen del host (los arranques en
// paralelo, el jailer). Sin esto, la única forma de saber con qué topes corría
// un daemon era leer el entorno de su proceso y repetir de cabeza las reglas
// de cada función; ahora GET /info (tuning) y `kling doctor` lo dicen.
//
// Se calcula en cada llamada: son lecturas del entorno y alguna de /proc,
// nada que pese, y así no puede quedarse viejo respecto a lo que se aplica.
func (m *Manager) Ajustes() map[string]string {
	onOff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	high := gcDiskHighPct()
	jailer := onOff(m.jailerJailed)
	if m.JailerBlocked != "" {
		jailer = "blocked"
	}
	boost := "off"
	if hasta, plazo := politicaImpulso(); hasta {
		boost = plazo.String()
	}
	cow := ""
	if c := m.CoWInfo(); c != nil {
		cow = c.Mode
	}
	return map[string]string{
		"KLING_MIN_FREE_DISK_MIB":   strconv.FormatInt(minFreeDiskMiB(), 10),
		"KLING_MAX_DISK_MIB":        strconv.Itoa(maxDiskMiB()),
		"KLING_MIN_FREE_MIB":        strconv.Itoa(minFreeMiB()),
		"KLING_MAX_MEM_PRESSURE":    strconv.FormatFloat(maxMemPressure(), 'g', -1, 64),
		"KLING_MAX_SWAP_PCT":        strconv.FormatInt(maxSwapPct(), 10),
		"KLING_MIN_MEM_LEVEL":       strconv.Itoa(minMemLevel()),
		"KLING_SHARE_RESERVE_DIV":   strconv.Itoa(shareReserveDiv()),
		"KLING_MAX_MACHINES":        strconv.Itoa(maxMachines()),
		"KLING_MAX_PARALLEL_BOOT":   strconv.Itoa(maxParallelLaunch()),
		"KLING_GC_DISK_HIGH":        strconv.Itoa(high),
		"KLING_GC_DISK_TARGET":      strconv.Itoa(gcDiskTargetPct(high)),
		"KLING_FAILED_RETENTION":    failedRetention().String(),
		"KLING_JAILER":              jailer,
		"KLING_DIFF_FREEZE":         onOff(congelarEnDiff()),
		"KLING_SQUEEZE_BEFORE_DUMP": onOff(!globoSinEstadisticas && os.Getenv("KLING_SQUEEZE_BEFORE_DUMP") != "0"),
		"KLING_READY_BOOST":         boost,
		"cow":                       cow,
	}
}
