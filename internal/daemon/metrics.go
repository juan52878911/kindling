package daemon

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
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
	fmt.Fprintln(w, "# HELP kling_machines Number of microVMs by state.")
	fmt.Fprintln(w, "# TYPE kling_machines gauge")
	for _, st := range []api.State{api.StateRunning, api.StatePaused, api.StateWarm, api.StateCreated, api.StateStopped, api.StateFailed} {
		fmt.Fprintf(w, "kling_machines{state=%q} %d\n", st, ps.ByState[string(st)])
	}

	// Memoria del host (0 si no hay /proc, p. ej. en dev sobre macOS).
	fmt.Fprintln(w, "# HELP kling_available_mib Host MemAvailable in MiB (/proc/meminfo).")
	fmt.Fprintln(w, "# TYPE kling_available_mib gauge")
	fmt.Fprintf(w, "kling_available_mib %d\n", ps.AvailableMiB)
	fmt.Fprintln(w, "# HELP kling_free_mib Host MemFree in MiB (/proc/meminfo).")
	fmt.Fprintln(w, "# TYPE kling_free_mib gauge")
	fmt.Fprintf(w, "kling_free_mib %d\n", ps.FreeMiB)

	fmt.Fprintln(w, "# HELP kling_total_pss_mib Sum of PSS across live microVMs, in MiB.")
	fmt.Fprintln(w, "# TYPE kling_total_pss_mib gauge")
	fmt.Fprintf(w, "kling_total_pss_mib %d\n", ps.TotalPSSMiB)

	// PSS por microVM viva. PSS y no RSS: con el mem.file compartido por COW
	// entre copias del mismo snapshot, el RSS cuenta el fichero entero en cada
	// una y las copias mienten; el PSS es la única cifra que suma de verdad.
	fmt.Fprintln(w, "# HELP kling_machine_pss_mib PSS of each live microVM, in MiB (/proc/<pid>/smaps_rollup).")
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

	escribirTelemetria(w, s.mgr.Telemetria(), s.bus.Descartados())
}

// escribirTelemetria escribe los contadores del ciclo de vida: lo que se ve
// cuando las cosas van mal (fallos, rechazos, latencias) y lo que el
// vigilante hace por su cuenta. Las series se emiten siempre, aunque estén a
// 0, por lo mismo que kling_machines.
func escribirTelemetria(w io.Writer, t machine.Telemetria, descartados int64) {
	ops := []string{machine.OpRun, machine.OpThaw, machine.OpFreeze}
	fmt.Fprintln(w, "# HELP kling_operations_total Lifecycle operations by result (rejections and caller errors are not failures).")
	fmt.Fprintln(w, "# TYPE kling_operations_total counter")
	for _, op := range ops {
		fmt.Fprintf(w, "kling_operations_total{op=%q,result=\"ok\"} %d\n", op, t.OK[op])
		fmt.Fprintf(w, "kling_operations_total{op=%q,result=\"error\"} %d\n", op, t.Fallos[op])
	}

	fmt.Fprintln(w, "# HELP kling_admission_rejections_total Requests refused for lack of room: 409 machine limit, 503 disk, 507 memory.")
	fmt.Fprintln(w, "# TYPE kling_admission_rejections_total counter")
	for _, c := range []int{api.StatusMachineLimit, api.StatusDiskFull, api.StatusInsufficientMemory} {
		fmt.Fprintf(w, "kling_admission_rejections_total{code=\"%d\"} %d\n", c, t.Rechazos[c])
	}

	// Histograma con los cubos ACUMULADOS, como manda el formato: cada "le"
	// cuenta todo lo que tardó eso o menos.
	fmt.Fprintln(w, "# HELP kling_operation_duration_ms Duration of successful lifecycle operations, in ms (boot: cold start; restore: run from a snapshot; thaw; resume: from paused; freeze).")
	fmt.Fprintln(w, "# TYPE kling_operation_duration_ms histogram")
	for _, k := range []string{machine.DurBoot, machine.DurRestore, machine.DurThaw, machine.DurResume, machine.DurFreeze} {
		h := t.Duraciones[k]
		var acum int64
		for i, l := range machine.LimitesDuracionMS {
			if i < len(h.Cubos) {
				acum += h.Cubos[i]
			}
			fmt.Fprintf(w, "kling_operation_duration_ms_bucket{kind=%q,le=\"%d\"} %d\n", k, l, acum)
		}
		fmt.Fprintf(w, "kling_operation_duration_ms_bucket{kind=%q,le=\"+Inf\"} %d\n", k, h.Cuenta)
		fmt.Fprintf(w, "kling_operation_duration_ms_sum{kind=%q} %d\n", k, h.Suma)
		fmt.Fprintf(w, "kling_operation_duration_ms_count{kind=%q} %d\n", k, h.Cuenta)
	}

	fmt.Fprintln(w, "# HELP kling_gc_evictions_total Dormant instances removed by the disk GC to free space.")
	fmt.Fprintln(w, "# TYPE kling_gc_evictions_total counter")
	fmt.Fprintf(w, "kling_gc_evictions_total %d\n", t.ExpulsionesGC)
	fmt.Fprintln(w, "# HELP kling_orphan_vmms_killed_total VMM processes with no machine that the daemon killed.")
	fmt.Fprintln(w, "# TYPE kling_orphan_vmms_killed_total counter")
	fmt.Fprintf(w, "kling_orphan_vmms_killed_total %d\n", t.HuerfanosMatados)
	fmt.Fprintln(w, "# HELP kling_events_dropped_total Events lost because a subscriber did not read them in time.")
	fmt.Fprintln(w, "# TYPE kling_events_dropped_total counter")
	fmt.Fprintf(w, "kling_events_dropped_total %d\n", descartados)

	if t.DiscoLibreMiB >= 0 {
		fmt.Fprintln(w, "# HELP kling_disk_free_mib Free disk under the daemon root, in MiB.")
		fmt.Fprintln(w, "# TYPE kling_disk_free_mib gauge")
		fmt.Fprintf(w, "kling_disk_free_mib %d\n", t.DiscoLibreMiB)
	}
	fmt.Fprintln(w, "# HELP kling_pending_mib Memory reserved by microVMs that are starting right now, in MiB.")
	fmt.Fprintln(w, "# TYPE kling_pending_mib gauge")
	fmt.Fprintf(w, "kling_pending_mib %d\n", t.PendienteMiB)
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
