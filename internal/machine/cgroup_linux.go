//go:build linux

package machine

import (
	"os"
	"syscall"
)

// cloneEnCgroup: Linux puede crear un proceso directamente dentro de un cgroup
// (clone3 con CLONE_INTO_CGROUP, kernel 5.7+).
const cloneEnCgroup = true

// enCgroup pide que el proceso nazca dentro del cgroup abierto en cg.
func enCgroup(a *syscall.SysProcAttr, cg *os.File) {
	a.UseCgroupFD = true
	a.CgroupFD = int(cg.Fd())
}
