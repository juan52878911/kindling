//go:build !darwin

package machine

// Los recursos de un VMM fuera de macOS, de /proc y del cgroup (ver
// recursos.go). Fuera de macOS no hay reparto entre núcleos ni energía.
// Donde /proc no existe, todo sale "no se sabe" sin romper.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// recursosVMM completa lo que cpuVMM no trae: despertares, disco y lo
// frenado por el techo de CPU. Aparte de cpuVMM porque ésta la llama también
// el vigilante de presión de memoria en cada vuelta, y esto son unas cuantas
// lecturas más de /proc que solo pide /metrics.
func (m *Manager) recursosVMM(id string, pid int, _ string, _ int, u *usoCPU) {
	if v, ok := cambiosVoluntarios(pid); ok {
		u.cambiosVoluntarios, u.conDespertares = v, true
	}
	if l, e, ok := discoProc(pid); ok {
		u.leidos, u.escritos, u.conDisco = l, e, true
	}
	// Solo el cgroup propio de la máquina: sin delegación el VMM vive en el
	// del daemon, y su cpu.stat no diría nada de ella.
	if m.cgroupRoot != "" {
		if d, ok := frenadoCgroup(m.dirCgroup(id)); ok {
			u.frenado, u.conFrenado = d, true
		}
	}
}

// cambiosVoluntarios suma voluntary_ctxt_switches de todos los hilos de pid:
// el de /proc/<pid>/status es solo el del hilo principal, y en Firecracker
// lo que despierta son los hilos de vCPU y de E/S.
func cambiosVoluntarios(pid int) (uint64, bool) {
	if pid <= 0 {
		return 0, false
	}
	dir := "/proc/" + strconv.Itoa(pid) + "/task"
	hilos, err := os.ReadDir(dir)
	if err != nil {
		return 0, false
	}
	var n uint64
	vistos := 0
	for _, h := range hilos {
		b, err := os.ReadFile(filepath.Join(dir, h.Name(), "status"))
		if err != nil {
			continue // un hilo que acaba de salir
		}
		if v, ok := campoUint(string(b), "voluntary_ctxt_switches:"); ok {
			n += v
			vistos++
		}
	}
	return n, vistos > 0
}

// discoProc es read_bytes y write_bytes de /proc/<pid>/io: lo que de verdad
// fue al dispositivo (rchar/wchar cuentan también lo servido por la caché).
func discoProc(pid int) (leidos, escritos uint64, ok bool) {
	if pid <= 0 {
		return 0, 0, false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/io")
	if err != nil {
		return 0, 0, false
	}
	l, ok1 := campoUint(string(b), "read_bytes:")
	e, ok2 := campoUint(string(b), "write_bytes:")
	return l, e, ok1 && ok2
}

// frenadoCgroup es throttled_usec del cpu.stat de dir: lo que cpu.max lleva
// parado al cgroup.
func frenadoCgroup(dir string) (time.Duration, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return 0, false
	}
	us, ok := campoUint(string(b), "throttled_usec")
	return time.Duration(us) * time.Microsecond, ok
}

// campoUint es el número tras la primera línea que empieza por prefijo, en
// los ficheros de /proc ("voluntary_ctxt_switches:\t12") y de cgroup
// ("throttled_usec 340").
func campoUint(s, prefijo string) (uint64, bool) {
	for l := range strings.SplitSeq(s, "\n") {
		resto, ok := strings.CutPrefix(l, prefijo)
		if !ok {
			continue
		}
		f := strings.Fields(resto)
		if len(f) == 0 {
			return 0, false
		}
		v, err := strconv.ParseUint(f[0], 10, 64)
		return v, err == nil
	}
	return 0, false
}

// usoPropio es la CPU y los despertares del daemon (pid es el suyo).
func usoPropio(pid int) (usoCPU, bool) {
	u, ok := cpuVMM(pid, "")
	if v, okv := cambiosVoluntarios(pid); okv {
		u.cambiosVoluntarios, u.conDespertares = v, true
	}
	return u, ok
}

// memoriaPropia es el PSS y el RSS de pid, en MiB.
func memoriaPropia(pid int) (huella, rss int64) {
	return pssMiB(pid), int64(procRSSMiB(pid))
}
