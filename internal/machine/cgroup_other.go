//go:build !linux

package machine

import (
	"os"
	"syscall"
)

// cloneEnCgroup: fuera de Linux no hay cgroups.
const cloneEnCgroup = false

func enCgroup(a *syscall.SysProcAttr, cg *os.File) {}
