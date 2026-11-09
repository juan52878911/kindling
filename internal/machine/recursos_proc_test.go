//go:build !darwin

package machine

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func plataformaConProc() bool { return runtime.GOOS == "linux" }

// throttled_usec del cpu.stat del cgroup de la máquina, y nada si el
// fichero no está (sin delegación, o el cgroup ya se borró).
func TestFrenadoCgroup(t *testing.T) {
	dir := t.TempDir()
	stat := "usage_usec 9000\nuser_usec 6000\nsystem_usec 3000\nnr_periods 40\nnr_throttled 12\nthrottled_usec 340000\n"
	if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	if d, ok := frenadoCgroup(dir); !ok || d != 340*time.Millisecond {
		t.Fatalf("frenado = %v, %v; quería 340ms", d, ok)
	}
	if _, ok := frenadoCgroup(t.TempDir()); ok {
		t.Fatal("sin cpu.stat dio frenado")
	}
}

// recursosVMM solo lee el cpu.stat del cgroup propio de la máquina: sin
// cgroupRoot (sin delegación) no hay frenado que decir.
func TestRecursosVMMCgroup(t *testing.T) {
	raiz := t.TempDir()
	m := &Manager{cgroupRoot: raiz}
	id := "0123456789abcdef0123"
	if err := os.MkdirAll(m.dirCgroup(id), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.dirCgroup(id), "cpu.stat"), []byte("throttled_usec 1500000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var u usoCPU
	m.recursosVMM(id, 0, "", 50, &u)
	if !u.conFrenado || u.frenado != 1500*time.Millisecond {
		t.Fatalf("con cgroup: %+v", u)
	}
	u = usoCPU{}
	(&Manager{}).recursosVMM(id, 0, "", 50, &u)
	if u.conFrenado {
		t.Fatalf("sin cgroupRoot: %+v", u)
	}
}

func TestCampoUint(t *testing.T) {
	s := "Name:\tfc\nvoluntary_ctxt_switches:\t1234\nnonvoluntary_ctxt_switches:\t5\n"
	if v, ok := campoUint(s, "voluntary_ctxt_switches:"); !ok || v != 1234 {
		t.Fatalf("voluntary = %d, %v", v, ok)
	}
	if _, ok := campoUint(s, "falta:"); ok {
		t.Fatal("un campo que no está")
	}
	if _, ok := campoUint("read_bytes: x\n", "read_bytes:"); ok {
		t.Fatal("un número que no lo es")
	}
}

// En Linux de verdad: los cambios voluntarios suman los de todos los hilos
// (el status del pid es solo el del hilo principal) y el disco sale de
// /proc/<pid>/io.
func TestRecursosDeProc(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("solo Linux")
	}
	pid := os.Getpid()
	v, ok := cambiosVoluntarios(pid)
	if !ok || v == 0 {
		t.Fatalf("cambios voluntarios = %d, %v", v, ok)
	}
	principal, _ := os.ReadFile("/proc/self/status")
	if p, _ := campoUint(string(principal), "voluntary_ctxt_switches:"); p > v {
		t.Errorf("el principal (%d) pasa de la suma de los hilos (%d)", p, v)
	}
	if _, _, ok := discoProc(pid); !ok {
		t.Error("sin /proc/<pid>/io")
	}
	if _, ok := cambiosVoluntarios(1 << 30); ok {
		t.Error("un PID que no existe tiene hilos")
	}
}
