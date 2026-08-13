package daemon

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/juan52878911/kindling/internal/api"
)

// handleProcStats devuelve el consumo de memoria en JSON, para `kling top`.
func (s *Server) handleProcStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.ProcStats())
}

// handleMetrics expone métricas en el formato de texto de Prometheus.
//
// Escrito a mano a propósito: son unas pocas líneas de Fprintf y no justifican
// arrastrar client_golang y su árbol de dependencias a un binario que hoy no
// tiene ninguna externa.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ps := s.mgr.ProcStats()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	// microVMs por estado. Se emiten los estados conocidos aunque estén a 0 para
	// que un scrape no vea "desaparecer" una serie cuando el contador cae a cero.
	fmt.Fprintln(w, "# HELP kling_machines Número de microVMs por estado.")
	fmt.Fprintln(w, "# TYPE kling_machines gauge")
	for _, st := range []api.State{api.StateRunning, api.StateWarm, api.StateCreated, api.StateStopped, api.StateFailed} {
		fmt.Fprintf(w, "kling_machines{state=%q} %d\n", st, ps.ByState[string(st)])
	}

	// Memoria del host (0 si no hay /proc, p. ej. en dev sobre macOS).
	fmt.Fprintln(w, "# HELP kling_available_mib MemAvailable del host en MiB (/proc/meminfo).")
	fmt.Fprintln(w, "# TYPE kling_available_mib gauge")
	fmt.Fprintf(w, "kling_available_mib %d\n", ps.AvailableMiB)
	fmt.Fprintln(w, "# HELP kling_free_mib MemFree del host en MiB (/proc/meminfo).")
	fmt.Fprintln(w, "# TYPE kling_free_mib gauge")
	fmt.Fprintf(w, "kling_free_mib %d\n", ps.FreeMiB)

	fmt.Fprintln(w, "# HELP kling_total_pss_mib Suma del PSS de las microVMs vivas, en MiB.")
	fmt.Fprintln(w, "# TYPE kling_total_pss_mib gauge")
	fmt.Fprintf(w, "kling_total_pss_mib %d\n", ps.TotalPSSMiB)

	// PSS por microVM viva. PSS y no RSS: con el mem.file compartido por COW
	// entre copias del mismo snapshot, el RSS cuenta el fichero entero en cada
	// una y las copias mienten; el PSS es la única cifra que suma de verdad.
	fmt.Fprintln(w, "# HELP kling_machine_pss_mib PSS de cada microVM viva, en MiB (/proc/<pid>/smaps_rollup).")
	fmt.Fprintln(w, "# TYPE kling_machine_pss_mib gauge")
	live := make([]api.ProcStat, 0, len(ps.Machines))
	for _, mc := range ps.Machines {
		if mc.PID > 0 {
			live = append(live, mc)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].PSSMiB > live[j].PSSMiB })
	for _, mc := range live {
		fmt.Fprintf(w, "kling_machine_pss_mib{id=%q,name=%q,service=%q,from=%q,state=%q} %d\n",
			mc.ID, esc(mc.Name), esc(mc.Service), esc(mc.From), mc.State, mc.PSSMiB)
	}
}

// esc escapa un valor de etiqueta según el formato de Prometheus: barra, comilla
// doble y salto de línea. Los ID son hex y los estados fijos, pero el nombre y el
// servicio los pone el usuario y pueden traer cualquier cosa.
func esc(v string) string {
	if !strings.ContainsAny(v, "\\\"\n") {
		return v
	}
	r := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n")
	return r.Replace(v)
}
