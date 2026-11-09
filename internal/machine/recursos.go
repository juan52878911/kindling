package machine

// Lo que gasta un VMM (o el propio daemon) además de memoria: CPU, despertares
// y E/S de disco, y lo que el techo de CPU lo lleva frenado. Es lo que hace
// falta para medir desde kindling los cambios de frecuencia de los vigilantes
// y del regulador (despertares), y el coste de disco de un thaw. Cada
// plataforma rellena lo que su sistema da: huella_vz.go (proc_pid_rusage en
// macOS) y recursos_proc.go (/proc y el cgroup en Linux).

import (
	"os"
	"time"
)

// usoCPU es lo que gastó un proceso (o varios sumados).
type usoCPU struct {
	total, p   time.Duration // CPU total y la de núcleos de rendimiento
	energiaNJ  uint64
	conNucleos bool // el kernel dio el reparto P/E (rusage v6)

	// Despertares. macOS cuenta los que sacan al paquete de CPU del reposo
	// (ri_pkg_idle_wkups) y los de interrupción (ri_interrupt_wkups), lo
	// mismo que la columna de powermetrics; Linux no tiene esa cifra y lo
	// más parecido son los cambios de contexto voluntarios de todos sus
	// hilos (un hilo que se duerme y lo despiertan).
	despReposo, despInterrupcion uint64
	cambiosVoluntarios           uint64
	conDespertares               bool

	// E/S de disco de verdad (no la que se queda en la caché de páginas):
	// ri_diskio_bytes* en macOS, read_bytes/write_bytes de /proc/<pid>/io.
	leidos, escritos uint64
	conDisco         bool

	// frenado es lo que el techo de CPU lleva parada la máquina (cpu.stat
	// del cgroup en Linux, el regulador de kling-vz/kling-hv en macOS).
	frenado    time.Duration
	conFrenado bool
}

func (u *usoCPU) sumar(o usoCPU) {
	u.total += o.total
	u.p += o.p
	u.energiaNJ += o.energiaNJ
	u.conNucleos = u.conNucleos || o.conNucleos
	u.despReposo += o.despReposo
	u.despInterrupcion += o.despInterrupcion
	u.cambiosVoluntarios += o.cambiosVoluntarios
	u.conDespertares = u.conDespertares || o.conDespertares
	u.leidos += o.leidos
	u.escritos += o.escritos
	u.conDisco = u.conDisco || o.conDisco
}

// RecursosProceso es lo que gasta el propio daemon, para /metrics: la
// cifra con la que medir si un vigilante menos o un intervalo más largo se
// nota de verdad.
type RecursosProceso struct {
	// HuellaMiB es su memoria: phys_footprint en macOS (lo que Monitor de
	// Actividad llama memoria), PSS en Linux. RSSMiB, la residente.
	HuellaMiB, RSSMiB int64
	CPU               time.Duration
	// Despertares, como en usoCPU: DespReposo y DespInterrupcion en macOS,
	// CambiosVoluntarios en Linux; ConDespertares dice si se pudo leer.
	DespReposo, DespInterrupcion, CambiosVoluntarios uint64
	ConDespertares                                   bool
}

// RecursosDaemon mide el proceso del daemon.
func RecursosDaemon() RecursosProceso {
	pid := os.Getpid()
	r := RecursosProceso{}
	r.HuellaMiB, r.RSSMiB = memoriaPropia(pid)
	if u, ok := usoPropio(pid); ok {
		r.CPU = u.total
		r.DespReposo, r.DespInterrupcion, r.CambiosVoluntarios = u.despReposo, u.despInterrupcion, u.cambiosVoluntarios
		r.ConDespertares = u.conDespertares
	}
	return r
}
