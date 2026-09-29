//go:build darwin

// Package footprint mide la memoria que ocupa una máquina en el host.
//
// El invitado NO vive en este proceso: Virtualization.framework lo aloja en un
// proceso auxiliar suyo (com.apple.Virtualization.VirtualMachine) por VM. Medir
// solo el ayudante daría 18 MiB para una máquina que ocupa 80. Se suma el
// phys_footprint de los dos, que es lo que macOS de verdad ha comprometido y lo
// que Activity Monitor llama "Memory": el equivalente al RSS de firecracker.
//
// Saber CUÁL de los auxiliares es el nuestro no es trivial: su padre es launchd
// y su proceso "responsable" es la app que lanzó al daemon, la misma para todas
// las máquinas. Lo que sí es único es la tubería de la consola: el framework
// pasa al auxiliar el MISMO extremo de tubería que le dimos, y el kernel expone
// su identidad (pipe_handle) en los dos procesos.
package footprint

/*
#include <libproc.h>
#include <sys/proc_info.h>
#include <sys/resource.h>
#include <unistd.h>
#include <mach/mach_time.h>
#include <string.h>
#include <stdlib.h>

static long long phys_footprint(int pid) {
	struct rusage_info_v4 ri;
	if (proc_pid_rusage(pid, RUSAGE_INFO_V4, (rusage_info_t *)&ri) != 0) return -1;
	return (long long)ri.ri_phys_footprint;
}

// cpu_ns es el tiempo de CPU (usuario + sistema) de pid en nanosegundos, o -1.
// proc_pid_rusage lo da en ticks de mach_absolute_time, no en nanosegundos
// (en Apple Silicon un tick son ~41,7 ns): sin convertirlo, 489 ms de CPU se
// leen como 12 ms (medido en un M4).
static long long cpu_ns(int pid) {
	struct rusage_info_v4 ri;
	if (proc_pid_rusage(pid, RUSAGE_INFO_V4, (rusage_info_t *)&ri) != 0) return -1;
	static mach_timebase_info_data_t tb;
	if (tb.denom == 0) mach_timebase_info(&tb);
	unsigned long long t = ri.ri_user_time + ri.ri_system_time;
	return (long long)((__uint128_t)t * tb.numer / tb.denom);
}

static int pipe_handle(int pid, int fd, unsigned long long *h) {
	struct pipe_fdinfo pi;
	int n = proc_pidfdinfo(pid, fd, PROC_PIDFDPIPEINFO, &pi, sizeof pi);
	if (n != (int)sizeof pi) return -1;
	*h = pi.pipeinfo.pipe_handle;
	return 0;
}

// holds_pipe dice si pid tiene abierto el extremo de tubería h.
static int holds_pipe(int pid, unsigned long long h) {
	int sz = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, NULL, 0);
	if (sz <= 0) return 0;
	struct proc_fdinfo *fds = malloc(sz);
	if (!fds) return 0;
	sz = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, fds, sz);
	int n = sz / (int)sizeof(struct proc_fdinfo), found = 0;
	for (int i = 0; i < n && !found; i++) {
		unsigned long long x;
		if (fds[i].proc_fdtype == PROX_FDTYPE_PIPE && pipe_handle(pid, fds[i].proc_fd, &x) == 0 && x == h) found = 1;
	}
	free(fds);
	return found;
}

// helpers rellena out con los pids de los auxiliares de Virtualization.framework.
static int helpers(int *out, int max) {
	int n = proc_listallpids(NULL, 0);
	if (n <= 0) return 0;
	pid_t *pids = calloc(n + 64, sizeof(pid_t));
	if (!pids) return 0;
	n = proc_listallpids(pids, (n + 64) * sizeof(pid_t));
	int k = 0;
	char path[PROC_PIDPATHINFO_MAXSIZE];
	for (int i = 0; i < n && k < max; i++) {
		if (pids[i] <= 0) continue;
		if (proc_pidpath(pids[i], path, sizeof path) <= 0) continue;
		if (strstr(path, "com.apple.Virtualization.VirtualMachine")) out[k++] = pids[i];
	}
	free(pids);
	return k;
}
*/
import "C"

import (
	"errors"
	"os"
	"sync"
	"time"
)

// Meter mide el ayudante y el auxiliar de su VM.
type Meter struct {
	mu     sync.Mutex
	anchor uint64 // pipe_handle de la consola de la VM; 0 = aún no hay VM
	helper int    // pid del auxiliar, cacheado
}

func NewMeter() *Meter { return &Meter{} }

// Track registra el descriptor de la tubería de consola que se entregó al
// framework al crear la VM.
func (m *Meter) Track(fd uintptr) {
	h, ok := pipeHandle(fd)
	if !ok {
		return
	}
	m.mu.Lock()
	m.anchor, m.helper = h, 0
	m.mu.Unlock()
}

func (m *Meter) findHelper() int {
	m.helper = buscarAuxiliar(m.anchor, m.helper)
	return m.helper
}

// pipeHandle es la identidad en el kernel del extremo de tubería fd de este
// proceso.
func pipeHandle(fd uintptr) (uint64, bool) {
	var h C.ulonglong
	if C.pipe_handle(C.int(os.Getpid()), C.int(fd), &h) != 0 {
		return 0, false
	}
	return uint64(h), true
}

// buscarAuxiliar devuelve el pid del auxiliar de Apple que tiene abierta la
// tubería anchor (0 si no hay), empezando por el que se encontró la última
// vez (cache).
func buscarAuxiliar(anchor uint64, cache int) int {
	if anchor == 0 {
		return 0
	}
	if cache > 0 && C.holds_pipe(C.int(cache), C.ulonglong(anchor)) == 1 {
		return cache
	}
	pids := make([]C.int, 1024)
	n := int(C.helpers(&pids[0], C.int(len(pids))))
	for i := 0; i < n; i++ {
		if C.holds_pipe(pids[i], C.ulonglong(anchor)) == 1 {
			return int(pids[i])
		}
	}
	return 0
}

// CPUTime es el tiempo de CPU que lleva consumido el auxiliar de Apple que
// aloja la VM: ahí corren sus vCPU. Es lo que mide el tope de CPU (cpu.go del
// servidor), el equivalente a la contabilidad de un cgroup en Linux.
func (m *Meter) CPUTime() (time.Duration, error) {
	m.mu.Lock()
	pid := m.findHelper()
	m.mu.Unlock()
	if pid <= 0 {
		return 0, errors.New("the VM's helper process is not running")
	}
	ns := C.cpu_ns(C.int(pid))
	if ns < 0 {
		return 0, errors.New("could not read the helper's CPU time")
	}
	return time.Duration(ns), nil
}

// Bytes devuelve phys_footprint del ayudante más el de su auxiliar.
func (m *Meter) Bytes() (uint64, error) {
	own := C.phys_footprint(C.int(os.Getpid()))
	if own < 0 {
		return 0, errors.New("could not read this process footprint")
	}
	total := uint64(own)
	m.mu.Lock()
	pid := m.findHelper()
	m.mu.Unlock()
	if pid > 0 {
		if f := C.phys_footprint(C.int(pid)); f > 0 {
			total += uint64(f)
		}
	}
	return total, nil
}
