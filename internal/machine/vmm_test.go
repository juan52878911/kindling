package machine

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestResolverVMM(t *testing.T) {
	exeDir := "/opt/kling/bin"
	junto := filepath.Join(exeDir, "kling-vz")
	look := func(n string) (string, error) {
		if n == "kling-vz" {
			return "/usr/local/bin/kling-vz", nil
		}
		return "", errors.New("no")
	}
	hayJunto := func(p string) bool { return p == junto }
	nada := func(string) bool { return false }
	noLook := func(string) (string, error) { return "", errors.New("no") }

	casos := []struct {
		nombre, backend, env, fcBin string
		existe                      func(string) bool
		look                        func(string) (string, error)
		quiero                      string
	}{
		{"firecracker usa el flag de siempre", BackendFirecracker, "", "/usr/bin/firecracker", nada, look, "/usr/bin/firecracker"},
		{"KLING_VMM ruta manda", BackendFirecracker, "/x/fc", "firecracker", nada, look, "/x/fc"},
		{"vz junto a kling gana al PATH", BackendVZ, "", "firecracker", hayJunto, look, junto},
		{"vz en el PATH", BackendVZ, "", "firecracker", nada, look, "/usr/local/bin/kling-vz"},
		{"vz sin encontrar: nombre a secas", BackendVZ, "", "firecracker", nada, noLook, "kling-vz"},
		{"KLING_VMM=vz es un nombre de backend", BackendVZ, "vz", "firecracker", hayJunto, look, junto},
		{"KLING_VMM ruta con vz", BackendVZ, "/tmp/fake-vz", "firecracker", hayJunto, look, "/tmp/fake-vz"},
		{"KLING_VMM de otro backend no cambia el binario", BackendFirecracker, "vz", "fc", nada, look, "fc"},
	}
	for _, c := range casos {
		if got := resolverVMM(c.backend, c.env, c.fcBin, exeDir, c.look, c.existe); got != c.quiero {
			t.Errorf("%s: %q, quiero %q", c.nombre, got, c.quiero)
		}
	}
}

func TestEvaluarNivelMemoria(t *testing.T) {
	if err := evaluarNivelMemoria(40, 15); err != nil {
		t.Fatalf("40%% con mínimo 15%%: %v", err)
	}
	if err := evaluarNivelMemoria(3, 0); err != nil {
		t.Fatal("mínimo 0 apaga la comprobación")
	}
	err := evaluarNivelMemoria(9, 15)
	var se *api.StatusError
	if !errors.As(err, &se) || se.Code != api.StatusInsufficientMemory {
		t.Fatalf("9%% con mínimo 15%% tiene que ser 507, es %v", err)
	}
	t.Setenv("KLING_MIN_MEM_LEVEL", "30")
	if minMemLevel() != 30 {
		t.Fatal("KLING_MIN_MEM_LEVEL no manda")
	}
	t.Setenv("KLING_MIN_MEM_LEVEL", "basura")
	if minMemLevel() != defaultMinMemLevel {
		t.Fatal("un valor ilegible vuelve al defecto")
	}
}

func TestLeerUint64LE(t *testing.T) {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, 16<<30) // 16 GiB: el byte alto es cero
	// syscall.Sysctl le quita el último byte a cero; aquí se simula.
	if got := leerUint64LE(string(b[:7])); got != 16<<30 {
		t.Fatalf("leerUint64LE = %d", got)
	}
}

// Los reenvíos viajan en state.json: una máquina que sobrevive a un reinicio
// del daemon (su ayudante sigue vivo) se sigue alcanzando por los mismos
// puertos sin volver a pedirlos.
func TestReenviosSobrevivenAlEstado(t *testing.T) {
	root := t.TempDir()
	in := []api.Machine{{ID: "aa11bb22cc33dd44", Name: "x", State: api.StateRunning,
		IP: "172.16.0.2", Forwards: map[string]string{"8080": "127.0.0.1:61234"}}}
	b, _ := json.Marshal(in)
	if err := os.WriteFile(filepath.Join(root, "state.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manager{root: root, byID: map[string]*api.Machine{}}
	m.load()
	mc := m.byID["aa11bb22cc33dd44"]
	if mc == nil || mc.Addr(api.GuestPort) != "127.0.0.1:61234" {
		t.Fatalf("tras cargar: %+v", mc)
	}
}

func TestObjetivoSinEstadisticas(t *testing.T) {
	casos := []struct {
		mem, max, quiero int
	}{
		{1024, 0, 512},     // la mitad
		{200, 0, 72},       // suelo de minResizeMiB: se le dejan 128
		{128, 0, 0},        // nada que apretar
		{1024, 2048, 1536}, // con techo: la línea base (1024) más la mitad
	}
	for _, c := range casos {
		got := objetivoSinEstadisticas(&api.Machine{MemMiB: c.mem, MemMaxMiB: c.max})
		if got != c.quiero {
			t.Errorf("mem %d techo %d: %d, quiero %d", c.mem, c.max, got, c.quiero)
		}
	}
}
