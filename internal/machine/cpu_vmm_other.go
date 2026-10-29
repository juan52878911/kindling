//go:build !darwin

package machine

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// usoCPU es lo que gastó un proceso. Fuera de macOS solo se sabe el total:
// ni reparto entre núcleos ni energía.
type usoCPU struct {
	total, p   time.Duration
	energiaNJ  uint64
	conNucleos bool
}

// memoriaEsHuella: en Linux la memoria por máquina es el PSS.
const memoriaEsHuella = false

// clkTck es USER_HZ, la unidad de utime y stime en /proc/<pid>/stat: 100 en
// todas las arquitecturas que soporta Linux.
const clkTck = 100

// cpuVMM lee utime + stime de /proc/<pid>/stat: la CPU del VMM, con sus vCPU
// (hilos de Firecracker).
func cpuVMM(pid int, _ string) (usoCPU, bool) {
	if pid <= 0 {
		return usoCPU{}, false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return usoCPU{}, false
	}
	return cpuDeStat(string(b))
}

// cpuDeStat saca utime y stime de una línea de /proc/<pid>/stat. El nombre del
// proceso va entre paréntesis y puede traer espacios: se cuenta desde el
// último ')'. Tras él, utime y stime son los campos 12 y 13 (14 y 15 de la
// línea entera).
func cpuDeStat(s string) (usoCPU, bool) {
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return usoCPU{}, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 13 {
		return usoCPU{}, false
	}
	ut, err1 := strconv.ParseUint(f[11], 10, 64)
	st, err2 := strconv.ParseUint(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return usoCPU{}, false
	}
	return usoCPU{total: time.Duration(ut+st) * time.Second / clkTck}, true
}
