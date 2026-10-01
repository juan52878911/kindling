package scripts

import (
	"strings"
	"testing"
)

// El contrato de runtime del invitado vive en sh y lo usan todas las bases
// (min, glibc, android). Quitar una de estas líneas no rompe ningún arranque de
// kindling: rompe, mucho después, el initdb de una imagen de Docker. Este test
// es esa alarma.
func TestMinimalInitRuntimeContract(t *testing.T) {
	for _, want := range []string{
		"fd:/proc/self/fd",
		"stdin:/proc/self/fd/0",
		"stdout:/proc/self/fd/1",
		"stderr:/proc/self/fd/2",
		"tmpfs -o mode=1777,nosuid,nodev tmpfs /dev/shm",
		"/proc/sys/kernel/hostname",
		`"127.0.0.1	localhost" >> /etc/hosts`,
		`"127.0.1.1	$HN" >> /etc/hosts`,
	} {
		if !strings.Contains(MinimalInit, want) {
			t.Errorf("minimal-init.sh sin %q", want)
		}
	}
	// El contrato va tras pivot_root (en la raíz nueva) y antes de /entrypoint.
	piv := strings.Index(MinimalInit, "pivot_root . rom")
	shm := strings.Index(MinimalInit, "/dev/shm")
	ep := strings.Index(MinimalInit, "exec /entrypoint")
	if !(piv < shm && shm < ep) {
		t.Errorf("orden: pivot_root %d, /dev/shm %d, /entrypoint %d", piv, shm, ep)
	}
}
