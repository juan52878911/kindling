package machine

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Con --new-pid-ns, jailer hace fork: el hijo (PID 1 de su espacio de PID)
// es Firecracker, y el padre apunta su PID en <root>/<exec>.pid y sale. El PID
// que se apunta en la máquina (para matarla, readoptarla en reconcile y
// barrer huérfanos) tiene que ser el del hijo, no el del jailer que ya salió.
// Aquí un jailer falso hace lo mismo con un sleep.
func TestSpawnJailedDevuelveElPIDDelHijoEnSuPidNS(t *testing.T) {
	m := newTestManager(t)
	m.fcBin = "/bin/sleep"
	m.priv = &Privileges{}
	argv, err := m.jailerArgv("abcdef0123456789", "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv[:slices.Index(argv, "--")], "--new-pid-ns") {
		t.Fatalf("jailer sin --new-pid-ns: %v", argv)
	}

	falso := filepath.Join(t.TempDir(), "jailer")
	script := `#!/bin/sh
while [ $# -gt 0 ]; do
  case "$1" in
    --id) id=$2; shift;;
    --chroot-base-dir) base=$2; shift;;
    --exec-file) exe=$2; shift;;
    --) break;;
  esac
  shift
done
root="$base/firecracker/$id/root"
mkdir -p "$root/run"
sleep 30 </dev/null >/dev/null 2>&1 &
echo $! > "$root/$(basename "$exe").pid"
exit 0
`
	if err := os.WriteFile(falso, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KLING_JAILER_BIN", falso)

	id := "1a1a000000000002"
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	pid, _, _, err := m.spawnJailed(id, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	b, err := os.ReadFile(filepath.Join(m.jailRoot(id), "sleep.pid"))
	if err != nil {
		t.Fatal(err)
	}
	hijo, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid != hijo {
		t.Fatalf("spawnJailed = pid %d, quería el del hijo %d (el jailer ya salió)", pid, hijo)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("el PID devuelto no está vivo: %v", err)
	}
}
