package machine

import (
	"os"
	"os/exec"
)

// debugfsBin localiza debugfs, que en Debian vive en /sbin y no siempre está en
// el PATH de un servicio de systemd.
func debugfsBin() string {
	if bin, err := exec.LookPath("debugfs"); err == nil {
		return bin
	}
	for _, p := range []string{"/sbin/debugfs", "/usr/sbin/debugfs"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}
