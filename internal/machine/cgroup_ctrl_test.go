package machine

import (
	"os"
	"path/filepath"
	"strconv"
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
		dir, warn := m.crearCgroup(id, 50, 1024)
		if warn != "" {
			t.Fatal(warn)
		}
		mem, errMem := os.ReadFile(filepath.Join(dir, "memory.max"))
		pids, errPids := os.ReadFile(filepath.Join(dir, "pids.max"))
		if !delegados {
			if errMem == nil || errPids == nil {
				t.Errorf("sin controladores delegados no se escribe nada: memory.max=%q pids.max=%q", mem, pids)
			}
			continue
		}
		quiero := strconv.Itoa((1024 + margenMemoriaVMM(1024)) << 20)
		if string(mem) != quiero {
			t.Errorf("memory.max=%q, quería %s (RAM + margen del VMM)", mem, quiero)
		}
		if string(pids) != strconv.Itoa(pidsMaxVMM) {
			t.Errorf("pids.max=%q", pids)
		}
		if cpu, _ := os.ReadFile(filepath.Join(dir, "cpu.max")); string(cpu) != "50000 100000" {
			t.Errorf("cpu.max=%q", cpu)
		}
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
