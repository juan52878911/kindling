//go:build darwin

package machine

// La memoria de los procesos hijos de un VMM en macOS: kling-vz lanza el freno
// del tope de CPU (y, hasta que el daemon publicó los dorados, el custodio de
// snapshots), cada uno con su runtime de Go y unos MiB de huella que el VMM no
// cuenta en su /kling/stats. Se leen aquí, desde el daemon, con proc_info (sin
// cgo, como procesos_vz.go): proc_listpids por padre y proc_pid_rusage.

import (
	"encoding/binary"
	"syscall"
	"unsafe"
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
)

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
