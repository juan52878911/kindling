package machine

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// `cpuset` contiene "cpu" como subcadena. Con strings.Contains, un anfitrion
// donde cpuset este delegado y cpu NO daba un falso positivo: se saltaba el
// `+cpu` y a partir de ahi ninguna microVM tenia techo de CPU — en silencio, y
// justo en los anfitriones donde el techo mas falta hace.
func TestControladorPresenteNoConfundeCpuConCpuset(t *testing.T) {
	casos := []struct {
		lista  string
		quiero string
		hay    bool
	}{
		{"cpuset cpu io memory pids", "cpu", true},
		{"cpu", "cpu", true},
		{"memory cpu", "cpu", true},
		{"cpuset io memory pids", "cpu", false}, // el caso del fallo
		{"cpuset", "cpu", false},
		{"", "cpu", false},
		{"cpuacct", "cpu", false},
		{"  cpuset   cpu  ", "cpu", true}, // espacios de sobra
	}
	for _, c := range casos {
		if got := controladorPresente(c.lista, c.quiero); got != c.hay {
			t.Errorf("controladorPresente(%q, %q) = %v, esperaba %v", c.lista, c.quiero, got, c.hay)
		}
	}
}

// El cgroup de cada VMM acota también su memoria (la del invitado más el
// margen del VMM) y sus procesos, cuando esos controladores están delegados;
// si no lo están, no se escriben (y el techo de CPU sigue).
func TestCgroupConTechoDeMemoriaYProcesos(t *testing.T) {
	for _, delegados := range []bool{true, false} {
		root := t.TempDir()
		m := &Manager{cgroupRoot: root, cgroupMemoria: delegados, cgroupProcesos: delegados}
		id := "abcdef0123456789"
		// Como en cgroupfs, memory.swap.max existe si el kernel cuenta el
		// swap: el daemon lo escribe, nunca lo crea.
		if err := os.MkdirAll(m.dirCgroup(id), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(m.dirCgroup(id), "memory.swap.max"), []byte("max"), 0o644); err != nil {
			t.Fatal(err)
		}
		dir, warn := m.crearCgroup(id, 50, 1024)
		if warn != "" {
			t.Fatal(warn)
		}
		mem, errMem := os.ReadFile(filepath.Join(dir, "memory.max"))
		pids, errPids := os.ReadFile(filepath.Join(dir, "pids.max"))
		swap, _ := os.ReadFile(filepath.Join(dir, "memory.swap.max"))
		if !delegados {
			if errMem == nil || errPids == nil || string(swap) != "max" {
				t.Errorf("sin controladores delegados no se escribe nada: memory.max=%q pids.max=%q memory.swap.max=%q", mem, pids, swap)
			}
			continue
		}
		quiero := strconv.Itoa((1024 + margenMemoriaVMM(1024)) << 20)
		if string(mem) != quiero {
			t.Errorf("memory.max=%q, quería %s (RAM + margen del VMM)", mem, quiero)
		}
		if string(swap) != quiero {
			t.Errorf("memory.swap.max=%q, quería %s: memory.max no acota el swap", swap, quiero)
		}
		if string(pids) != strconv.Itoa(pidsMaxVMM) {
			t.Errorf("pids.max=%q", pids)
		}
		if cpu, _ := os.ReadFile(filepath.Join(dir, "cpu.max")); string(cpu) != "50000 100000" {
			t.Errorf("cpu.max=%q", cpu)
		}
	}
}

// Sin contabilidad de swap (no hay memory.swap.max) no se crea el fichero ni
// falla el cgroup.
func TestCgroupSinSwapNoLoCrea(t *testing.T) {
	m := &Manager{cgroupRoot: t.TempDir(), cgroupMemoria: true}
	dir, warn := m.crearCgroup("abcdef0123456789", 50, 512)
	if warn != "" {
		t.Fatal(warn)
	}
	if _, err := os.Stat(filepath.Join(dir, "memory.swap.max")); err == nil {
		t.Error("se creó memory.swap.max")
	}
}

// Con techo de memoria (MemMaxMiB) el cgroup admite el techo, no la memoria
// de arranque: el globo puede devolverle hasta él sin reiniciar.
func TestMemoriaCgroupEsElTecho(t *testing.T) {
	if got := memoriaCgroup(&api.Machine{MemMiB: 512, MemMaxMiB: 2048}); got != 2048 {
		t.Errorf("con techo: %d", got)
	}
	if got := memoriaCgroup(&api.Machine{MemMiB: 512}); got != 512 {
		t.Errorf("sin techo: %d", got)
	}
	if m := margenMemoriaVMM(512); m < 32 || m > 512/4+64 {
		t.Errorf("margen de %d MiB para 512 MiB: fuera de lo razonable", m)
	}
}

// Un techo de memoria o de procesos que no se puede escribir no deja al VMM
// sin cgroup: el de CPU, que ya tenía antes de ellos, se sigue aplicando y el
// proceso entra en el cgroup.
func TestCgroupSinTechoDeMemoriaSigueConElDeCPU(t *testing.T) {
	m := &Manager{cgroupRoot: t.TempDir(), cgroupMemoria: true, cgroupProcesos: true}
	id := "abcdef0123456789"
	// memory.max y pids.max que no se pueden escribir.
	for _, f := range []string{"memory.max", "pids.max"} {
		if err := os.MkdirAll(filepath.Join(m.dirCgroup(id), f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if warn := m.limitCPU(id, 4242, 50, 512); warn != "" {
		t.Fatalf("aviso: %s", warn)
	}
	if cpu, _ := os.ReadFile(filepath.Join(m.dirCgroup(id), "cpu.max")); string(cpu) != "50000 100000" {
		t.Errorf("cpu.max=%q", cpu)
	}
	if procs, _ := os.ReadFile(filepath.Join(m.dirCgroup(id), "cgroup.procs")); string(procs) != "4242" {
		t.Errorf("cgroup.procs=%q: el VMM se quedó fuera del cgroup", procs)
	}
}

// Cada techo va por su lado: un memory.max que no se puede escribir no deja al
// VMM sin el de swap ni sin el de procesos (antes, el primer fallo cortaba los
// demás).
func TestCgroupUnTechoFallidoNoSeLlevaLosDemas(t *testing.T) {
	m := &Manager{cgroupRoot: t.TempDir(), cgroupMemoria: true, cgroupProcesos: true}
	id := "abcdef0123456789"
	dir := m.dirCgroup(id)
	// memory.max que no se puede escribir; memory.swap.max existe, como en
	// cgroupfs con contabilidad de swap.
	if err := os.MkdirAll(filepath.Join(dir, "memory.max"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.swap.max"), []byte("max"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := m.escribirLimitesMemoria(dir, 512)
	if err == nil || !strings.Contains(err.Error(), "memory.max") {
		t.Errorf("err = %v: no cuenta el memory.max que falló", err)
	}
	quiero := strconv.Itoa((512 + margenMemoriaVMM(512)) << 20)
	if swap, _ := os.ReadFile(filepath.Join(dir, "memory.swap.max")); string(swap) != quiero {
		t.Errorf("memory.swap.max=%q, quería %s", swap, quiero)
	}
	if pids, _ := os.ReadFile(filepath.Join(dir, "pids.max")); string(pids) != strconv.Itoa(pidsMaxVMM) {
		t.Errorf("pids.max=%q: el fallo de memory.max se llevó el techo de procesos", pids)
	}
	if warn := m.limitCPU(id, 4242, 50, 512); warn != "" {
		t.Fatalf("aviso: %s", warn)
	}
	if procs, _ := os.ReadFile(filepath.Join(dir, "cgroup.procs")); string(procs) != "4242" {
		t.Errorf("cgroup.procs=%q: el VMM se quedó fuera del cgroup", procs)
	}
}
