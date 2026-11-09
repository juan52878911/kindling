package daemon

import (
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"time"

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
	// Un inquilino ve las cifras del host, no las máquinas: las series por
	// máquina llevan el ID y el nombre de todas, también las ajenas.
	if _, filtrar := inquilinoDe(r); filtrar {
		ps.Machines = nil
	}
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

	escribirPorMaquina(w, ps, time.Now())
	escribirTelemetria(w, s.mgr.Telemetria(), s.bus.Descartados())
	escribirDaemon(w, machine.RecursosDaemon(), runtime.NumGoroutine(), s.mgr.EnBateria())
}

// escribirDaemon escribe lo que gasta el propio daemon y si va con batería:
// con qué medir que un vigilante menos o un intervalo más largo se nota.
func escribirDaemon(w io.Writer, r machine.RecursosProceso, goroutines int, bateria bool) {
	fmt.Fprintln(w, "# HELP kling_daemon_footprint_mib Memory of the daemon process, in MiB (macOS: phys_footprint; Linux: PSS).")
	fmt.Fprintln(w, "# TYPE kling_daemon_footprint_mib gauge")
	fmt.Fprintf(w, "kling_daemon_footprint_mib %d\n", r.HuellaMiB)
	fmt.Fprintln(w, "# HELP kling_daemon_rss_mib Resident memory of the daemon process, in MiB.")
	fmt.Fprintln(w, "# TYPE kling_daemon_rss_mib gauge")
	fmt.Fprintf(w, "kling_daemon_rss_mib %d\n", r.RSSMiB)
	fmt.Fprintln(w, "# HELP kling_daemon_cpu_seconds_total CPU time used by the daemon process since it started.")
	fmt.Fprintln(w, "# TYPE kling_daemon_cpu_seconds_total counter")
	fmt.Fprintf(w, "kling_daemon_cpu_seconds_total %.3f\n", r.CPU.Seconds())
	if r.ConDespertares {
		fmt.Fprintln(w, "# HELP kling_daemon_wakeups_total Wakeups of the daemon process since it started (macOS: kind pkg_idle and interrupt, from rusage; Linux: kind voluntary_switch, voluntary context switches of all its threads).")
		fmt.Fprintln(w, "# TYPE kling_daemon_wakeups_total counter")
		escribirDespertares(w, "kling_daemon_wakeups_total", "", r.DespReposo, r.DespInterrupcion, r.CambiosVoluntarios)
	}
	fmt.Fprintln(w, "# HELP kling_daemon_goroutines Goroutines alive in the daemon.")
	fmt.Fprintln(w, "# TYPE kling_daemon_goroutines gauge")
	fmt.Fprintf(w, "kling_daemon_goroutines %d\n", goroutines)
	fmt.Fprintln(w, "# HELP kling_battery_on 1 while the daemon's battery mode is in force (on battery power with daemon.battery=auto, or daemon.battery=on), else 0.")
	fmt.Fprintln(w, "# TYPE kling_battery_on gauge")
	b := 0
	if bateria {
		b = 1
	}
	fmt.Fprintf(w, "kling_battery_on %d\n", b)
}

// escribirDespertares escribe una línea por clase de despertar que la
// plataforma da: pkg_idle e interrupt en macOS, voluntary_switch en Linux.
// etiquetas va delante de kind (`id="…",name="…",` o vacío).
func escribirDespertares(w io.Writer, serie, etiquetas string, reposo, interrupcion, voluntarios uint64) {
	if reposo > 0 || interrupcion > 0 {
		fmt.Fprintf(w, "%s{%skind=\"pkg_idle\"} %d\n", serie, etiquetas, reposo)
		fmt.Fprintf(w, "%s{%skind=\"interrupt\"} %d\n", serie, etiquetas, interrupcion)
	}
	if voluntarios > 0 {
		fmt.Fprintf(w, "%s{%skind=\"voluntary_switch\"} %d\n", serie, etiquetas, voluntarios)
	}
}

// escribirPorMaquina escribe lo que hace /metrics útil también en macOS (5.1),
// donde no hay /proc: la memoria del host según el sistema, la huella de cada
// máquina, su CPU (repartida entre núcleos P y E en Apple Silicon), su
// energía y cuánto lleva en su estado.
func escribirPorMaquina(w io.Writer, ps api.ProcStats, ahora time.Time) {
	fmt.Fprintln(w, "# HELP kling_host_mem_available_mib Memory the host can still hand out, in MiB (Linux: MemAvailable; macOS: kern.memorystatus_level of hw.memsize).")
	fmt.Fprintln(w, "# TYPE kling_host_mem_available_mib gauge")
	fmt.Fprintf(w, "kling_host_mem_available_mib %d\n", ps.AvailableMiB)

	var vivas, todas []api.ProcStat
	for _, mc := range ps.Machines {
		todas = append(todas, mc)
		if mc.PID > 0 {
			vivas = append(vivas, mc)
		}
	}
	sort.Slice(vivas, func(i, j int) bool { return vivas[i].ID < vivas[j].ID })
	sort.Slice(todas, func(i, j int) bool { return todas[i].ID < todas[j].ID })

	if ps.MemoryKind == "footprint" {
		fmt.Fprintln(w, "# HELP kling_machine_footprint_mib Physical footprint of each live microVM (VMM, Apple's helper and children), in MiB.")
		fmt.Fprintln(w, "# TYPE kling_machine_footprint_mib gauge")
		for _, mc := range vivas {
			fmt.Fprintf(w, "kling_machine_footprint_mib{id=%q,name=%q,state=%q} %d\n", mc.ID, esc(mc.Name), mc.State, mc.FootprintMiB)
		}
	}

	fmt.Fprintln(w, "# HELP kling_machine_cpu_seconds_total CPU time used by each live microVM's VMM since it started; core is p or e on Apple Silicon, all where the split is not known.")
	fmt.Fprintln(w, "# TYPE kling_machine_cpu_seconds_total counter")
	for _, mc := range vivas {
		if mc.CPUSeconds <= 0 {
			continue
		}
		if mc.CoreSplit {
			fmt.Fprintf(w, "kling_machine_cpu_seconds_total{id=%q,name=%q,core=%q} %.3f\n", mc.ID, esc(mc.Name), "p", mc.CPUPSeconds)
			fmt.Fprintf(w, "kling_machine_cpu_seconds_total{id=%q,name=%q,core=%q} %.3f\n", mc.ID, esc(mc.Name), "e", max(mc.CPUSeconds-mc.CPUPSeconds, 0))
		} else {
			fmt.Fprintf(w, "kling_machine_cpu_seconds_total{id=%q,name=%q,core=%q} %.3f\n", mc.ID, esc(mc.Name), "all", mc.CPUSeconds)
		}
	}

	fmt.Fprintln(w, "# HELP kling_machine_energy_joules_total Energy macOS attributes to each live microVM's processes since they started, in joules.")
	fmt.Fprintln(w, "# TYPE kling_machine_energy_joules_total counter")
	for _, mc := range vivas {
		if mc.EnergyJoules > 0 {
			fmt.Fprintf(w, "kling_machine_energy_joules_total{id=%q,name=%q} %.3f\n", mc.ID, esc(mc.Name), mc.EnergyJoules)
		}
	}

	fmt.Fprintln(w, "# HELP kling_machine_wakeups_total Wakeups of each live microVM's VMM since it started (macOS: kind pkg_idle and interrupt, with Apple's helper and children; Linux: kind voluntary_switch, voluntary context switches of all its threads).")
	fmt.Fprintln(w, "# TYPE kling_machine_wakeups_total counter")
	for _, mc := range vivas {
		escribirDespertares(w, "kling_machine_wakeups_total", fmt.Sprintf("id=%q,name=%q,", mc.ID, esc(mc.Name)),
			mc.WakeupsPkgIdle, mc.WakeupsInterrupt, mc.VoluntarySwitches)
	}

	fmt.Fprintln(w, "# HELP kling_machine_disk_bytes_total Bytes each live microVM's VMM read from or wrote to disk since it started (not counting what the page cache served).")
	fmt.Fprintln(w, "# TYPE kling_machine_disk_bytes_total counter")
	for _, mc := range vivas {
		if mc.DiskReadBytes == 0 && mc.DiskWriteBytes == 0 {
			continue
		}
		fmt.Fprintf(w, "kling_machine_disk_bytes_total{id=%q,name=%q,op=\"read\"} %d\n", mc.ID, esc(mc.Name), mc.DiskReadBytes)
		fmt.Fprintf(w, "kling_machine_disk_bytes_total{id=%q,name=%q,op=\"write\"} %d\n", mc.ID, esc(mc.Name), mc.DiskWriteBytes)
	}

	fmt.Fprintln(w, "# HELP kling_machine_cpu_throttled_seconds_total Time each live microVM has been held back by its CPU cap (Linux: throttled_usec of its cgroup; macOS: kling-vz/kling-hv regulator, only machines with a cap on kling-hv).")
	fmt.Fprintln(w, "# TYPE kling_machine_cpu_throttled_seconds_total counter")
	for _, mc := range vivas {
		if mc.CPUThrottledSeconds != nil {
			fmt.Fprintf(w, "kling_machine_cpu_throttled_seconds_total{id=%q,name=%q} %.3f\n", mc.ID, esc(mc.Name), *mc.CPUThrottledSeconds)
		}
	}

	fmt.Fprintln(w, "# HELP kling_machine_state_seconds How long each machine has been in its current state, in seconds.")
	fmt.Fprintln(w, "# TYPE kling_machine_state_seconds gauge")
	for _, mc := range todas {
		if mc.StateSince == nil {
			continue
		}
		fmt.Fprintf(w, "kling_machine_state_seconds{id=%q,name=%q,state=%q} %.0f\n", mc.ID, esc(mc.Name), mc.State,
			max(ahora.Sub(*mc.StateSince).Seconds(), 0))
	}
}

// escribirTelemetria escribe los contadores del ciclo de vida: lo que se ve
// cuando las cosas van mal (fallos, rechazos, latencias) y lo que el
// vigilante hace por su cuenta. Las series se emiten siempre, aunque estén a
// 0, por lo mismo que kling_machines.
func escribirTelemetria(w io.Writer, t machine.Telemetria, descartados int64) {
	ops := []string{machine.OpRun, machine.OpThaw, machine.OpFreeze, machine.OpStart}
	fmt.Fprintln(w, "# HELP kling_operations_total Lifecycle operations by result (rejections and caller errors are not failures).")
	fmt.Fprintln(w, "# TYPE kling_operations_total counter")
	for _, op := range ops {
		fmt.Fprintf(w, "kling_operations_total{op=%q,result=\"ok\"} %d\n", op, t.OK[op])
		fmt.Fprintf(w, "kling_operations_total{op=%q,result=\"error\"} %d\n", op, t.Fallos[op])
	}

	fmt.Fprintln(w, "# HELP kling_admission_rejections_total Requests refused for lack of room: 409 machine limit, 429 tenant quota, 503 disk, 507 memory.")
	fmt.Fprintln(w, "# TYPE kling_admission_rejections_total counter")
	for _, c := range []int{api.StatusMachineLimit, api.StatusTenantQuota, api.StatusDiskFull, api.StatusInsufficientMemory} {
		fmt.Fprintf(w, "kling_admission_rejections_total{code=\"%d\"} %d\n", c, t.Rechazos[c])
	}

	// Histograma con los cubos ACUMULADOS, como manda el formato: cada "le"
	// cuenta todo lo que tardó eso o menos.
	fmt.Fprintln(w, "# HELP kling_operation_duration_ms Duration of successful lifecycle operations, in ms (boot: cold start, run or start; restore: run from a snapshot; thaw; resume: from paused; freeze).")
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

	// La suma por fase de los despertares que salieron bien: dividida por
	// kling_operation_duration_ms_count de su clase, la media de cada fase.
	fmt.Fprintln(w, "# HELP kling_wake_phase_ms_total Time spent in each phase of successful wakes, in ms (kind thaw: from a snapshot on disk; resume: from paused). Divide by kling_operation_duration_ms_count of the same kind for the mean per wake.")
	fmt.Fprintln(w, "# TYPE kling_wake_phase_ms_total counter")
	for _, k := range []string{machine.DurThaw, machine.DurResume} {
		for _, f := range machine.FasesDespertar {
			fmt.Fprintf(w, "kling_wake_phase_ms_total{kind=%q,phase=%q} %.3f\n", k, f, t.FasesMS[k][f])
		}
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

	fmt.Fprintln(w, "# HELP kling_memory_pressure_level Host memory pressure the daemon acts on: 1 normal, 2 warn, 4 critical, 0 unknown or daemon.memory_pressure=off.")
	fmt.Fprintln(w, "# TYPE kling_memory_pressure_level gauge")
	fmt.Fprintf(w, "kling_memory_pressure_level %d\n", t.PresionNivel)
	fmt.Fprintln(w, "# HELP kling_memory_pressure_actions_total Idle machines the daemon squeezed or froze because of host memory pressure.")
	fmt.Fprintln(w, "# TYPE kling_memory_pressure_actions_total counter")
	fmt.Fprintf(w, "kling_memory_pressure_actions_total{action=\"squeeze\"} %d\n", t.PresionApretones)
	fmt.Fprintf(w, "kling_memory_pressure_actions_total{action=\"freeze\"} %d\n", t.PresionCongelaciones)
	fmt.Fprintln(w, "# HELP kling_memory_pressure_reclaimed_mib_total Memory those squeezes gave back to the host, in MiB.")
	fmt.Fprintln(w, "# TYPE kling_memory_pressure_reclaimed_mib_total counter")
	fmt.Fprintf(w, "kling_memory_pressure_reclaimed_mib_total %d\n", t.PresionDevueltaMiB)
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
