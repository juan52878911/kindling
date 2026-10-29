//go:build darwin

package machine

// La memoria de los procesos hijos de un VMM en macOS: kling-vz lanza el freno
// del tope de CPU (y, hasta que el daemon publicó los dorados, el custodio de
// snapshots), cada uno con su runtime de Go y unos MiB de huella que el VMM no
// cuenta en su /kling/stats. Se leen aquí, desde el daemon, con proc_info (sin
// cgo, como procesos_vz.go): proc_listpids por padre y proc_pid_rusage.

import (
	"context"
	"encoding/binary"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/juan52878911/kindling/internal/fc"
)

const (
	// procPPIDOnly es PROC_PPID_ONLY de <sys/proc_info.h>.
	procPPIDOnly = 6
	// procInfoCallPIDRusage es PROC_INFO_CALL_PIDRUSAGE (proc_pid_rusage).
	procInfoCallPIDRusage = 9
	// rusageInfoV0 es RUSAGE_INFO_V0: la versión más corta que ya lleva
	// ri_phys_footprint.
	rusageInfoV0 = 0
	// offPhysFootprint es dónde va ri_phys_footprint en rusage_info_v0:
	// ri_uuid (16 bytes) y siete uint64 delante.
	offPhysFootprint = 16 + 7*8
	// rusageInfoV6 (macOS 12+) añade lo que corrió en núcleos de
	// rendimiento y la energía. Desplazamientos de <sys/resource.h>,
	// comprobados en un M4 contra getrusage (ri_user_time) y entre sí.
	rusageInfoV6    = 6
	offUserTime     = 16  // ri_user_time (ticks de Mach)
	offSystemTime   = 24  // ri_system_time
	offUserPTime    = 304 // ri_user_ptime: la parte en núcleos P
	offSystemPTime  = 312 // ri_system_ptime
	offEnergyNJ     = 336 // ri_energy_nj
	tamRusageInfoV6 = 400 // rusage_info_v6 entero, con sus reservados
)

// usoCPU es lo que gastó un proceso (o varios sumados).
type usoCPU struct {
	total, p   time.Duration // CPU total y la de núcleos de rendimiento
	energiaNJ  uint64
	conNucleos bool // el kernel dio el reparto P/E (rusage v6)
}

func (u *usoCPU) sumar(o usoCPU) {
	u.total += o.total
	u.p += o.p
	u.energiaNJ += o.energiaNJ
	u.conNucleos = u.conNucleos || o.conNucleos
}

// ticksPorSegundo es la frecuencia de los ticks de Mach (hw.tbfrequency: 24
// MHz en Apple Silicon, 1 GHz en Intel, donde un tick es un ns).
var ticksPorSegundo = sync.OnceValue(func() uint64 {
	s, err := syscall.Sysctl("hw.tbfrequency")
	if err != nil {
		return 0
	}
	return leerUint64LE(s)
})

func ticksADuracion(t uint64) time.Duration {
	f := ticksPorSegundo()
	if f == 0 {
		return 0
	}
	return time.Duration(float64(t) / float64(f) * float64(time.Second))
}

// cpuProceso lee con proc_pid_rusage (v6) la CPU y la energía de pid.
func cpuProceso(pid int) (usoCPU, bool) {
	if pid <= 0 {
		return usoCPU{}, false
	}
	var buf [tamRusageInfoV6 + 64]byte
	_, _, e := syscall.Syscall6(syscall.SYS_PROC_INFO, procInfoCallPIDRusage, uintptr(pid), rusageInfoV6, 0,
		uintptr(unsafe.Pointer(&buf[0])), 0)
	if e != 0 {
		return usoCPU{}, false
	}
	u64 := func(off int) uint64 { return binary.LittleEndian.Uint64(buf[off:]) }
	return usoCPU{
		total:      ticksADuracion(u64(offUserTime) + u64(offSystemTime)),
		p:          ticksADuracion(u64(offUserPTime) + u64(offSystemPTime)),
		energiaNJ:  u64(offEnergyNJ),
		conNucleos: true,
	}, true
}

// cpuVMM es la CPU del VMM pid: la suya, la del auxiliar de Apple que corre
// los vCPU con kling-vz (con kling-hv corren en el propio proceso y no hay
// auxiliar) y la de sus hijos (el freno del tope de CPU).
func cpuVMM(pid int, sock string) (usoCPU, bool) {
	u, ok := cpuProceso(pid)
	if !ok {
		return usoCPU{}, false
	}
	if sock != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		e, err := fc.New(sock).KlingCPUState(ctx)
		cancel()
		if err == nil && e.HelperPID > 0 {
			if a, ok := cpuProceso(e.HelperPID); ok {
				u.sumar(a)
			}
		}
	}
	for _, h := range hijosDe(pid) {
		if c, ok := cpuProceso(h); ok {
			u.sumar(c)
		}
	}
	return u, true
}

// memoriaEsHuella: en macOS la memoria por máquina es la huella, no el PSS.
const memoriaEsHuella = true

// hijosDe son los PID cuyo padre es ppid.
func hijosDe(ppid int) []int {
	var buf [64]int32
	r, _, e := syscall.Syscall6(syscall.SYS_PROC_INFO, procInfoCallListPIDs, procPPIDOnly, uintptr(ppid), 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*4))
	if e != 0 {
		return nil
	}
	var out []int
	for _, p := range buf[:min(int(r)/4, len(buf))] {
		if p > 0 {
			out = append(out, int(p))
		}
	}
	return out
}

// huellaBytes es el phys_footprint de pid (lo que Monitor de Actividad llama
// memoria), o 0 si no se puede leer.
func huellaBytes(pid int) int64 {
	// rusage_info_v0 son 96 bytes; de sobra por si el kernel escribe más.
	var buf [256]byte
	_, _, e := syscall.Syscall6(syscall.SYS_PROC_INFO, procInfoCallPIDRusage, uintptr(pid), rusageInfoV0, 0,
		uintptr(unsafe.Pointer(&buf[0])), 0)
	if e != 0 {
		return 0
	}
	return int64(binary.LittleEndian.Uint64(buf[offPhysFootprint:]))
}

// huellaHijosBytes suma la huella de los hijos directos de pid.
func huellaHijosBytes(pid int) int64 {
	if pid <= 0 {
		return 0
	}
	var n int64
	for _, h := range hijosDe(pid) {
		n += huellaBytes(h)
	}
	return n
}
