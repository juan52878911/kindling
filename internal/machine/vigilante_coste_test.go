package machine

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// El vigilante solo vuelve a medir el disco de las que pueden estar
// escribiendo. Una congelada se mide una vez y no se recorre en cada vuelta;
// cuando cambia de estado, sí.
func TestRefreshDiskUsageNoRecorreLasDormidasCadaVuelta(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	m.byID["w"] = &api.Machine{ID: "w", Name: "w", State: api.StateWarm}
	m.byID["r"] = &api.Machine{ID: "r", Name: "r", State: api.StateRunning}
	m.mu.Unlock()
	writeMachineFile(t, m, "w", "mem.file", 64<<10)
	writeMachineFile(t, m, "r", "overlay.ext4", 64<<10)

	m.refreshDiskUsage()
	disco := func(id string) int64 {
		mc, _ := m.Get(id)
		return mc.DiskBytes
	}
	w0, r0 := disco("w"), disco("r")
	if w0 == 0 || r0 == 0 {
		t.Fatalf("la primera vuelta mide todas: warm=%d running=%d", w0, r0)
	}

	writeMachineFile(t, m, "w", "mem.file", 512<<10)
	writeMachineFile(t, m, "r", "overlay.ext4", 512<<10)
	m.refreshDiskUsage()
	if disco("w") != w0 {
		t.Errorf("volvió a recorrer una congelada sin cambio de estado: %d -> %d", w0, disco("w"))
	}
	if disco("r") <= r0 {
		t.Errorf("no vio crecer la que corre: %d -> %d", r0, disco("r"))
	}

	// Cambia de estado: toca medirla otra vez.
	m.mu.Lock()
	m.byID["w"].State = api.StateStopped
	m.mu.Unlock()
	m.refreshDiskUsage()
	if disco("w") <= w0 {
		t.Errorf("tras cambiar de estado no se volvió a medir: %d -> %d", w0, disco("w"))
	}

	// Lo de una máquina que ya no existe no se queda en el mapa.
	m.mu.Lock()
	delete(m.byID, "w")
	m.mu.Unlock()
	m.refreshDiskUsage()
	m.mu.RLock()
	_, queda := m.discoMedido["w"]
	m.mu.RUnlock()
	if queda {
		t.Error("discoMedido conserva una máquina borrada")
	}
}

// Remove poda la numeración de vigías de "listo": antes cada máquina que
// pasaba por el daemon dejaba su entrada para siempre.
func TestRemovePodaLasVigiasDeListo(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	mc := m.addForTest(newID())
	mc.State = api.StateStopped
	// Una vigía que no llega a arrancar su goroutine de sondeo: lo que importa
	// es la entrada en el mapa.
	m.vigilarListo(mc.ID, nil)
	if _, ok := m.vigiasListo.Load(mc.ID); !ok {
		t.Fatal("la vigía no se apuntó")
	}
	if err := m.Remove(mc.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.vigiasListo.Load(mc.ID); ok {
		t.Error("Remove dejó la entrada de vigiasListo")
	}
}

// vmmSigue: con el escaneo de la vuelta casando (y el socket en su sitio) no
// hace falta preguntar por el proceso; sin casar, decide adopt como siempre.
func TestVmmSigueUsaElEscaneoDeLaVuelta(t *testing.T) {
	m := newTestManager(t)
	id := "beef000000000001"
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.dir(id), "fc.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Un PID que no es un VMM: adopt diría que no. Si vmmSigue dice que sí, es
	// que se fió del escaneo.
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Skip(err)
	}
	pid := cmd.Process.Pid
	if !m.vmmSigue(map[string]int{id: pid}, id, pid) {
		t.Error("con el escaneo casando y el socket presente debería darlo por vivo")
	}
	if m.vmmSigue(map[string]int{id: pid + 1}, id, pid) {
		t.Error("con otro PID en el escaneo, adopt manda: este no es un VMM")
	}
	if m.vmmSigue(map[string]int{}, id, pid) {
		t.Error("fuera del escaneo y sin VMM de verdad no puede estar vivo")
	}
	os.Remove(filepath.Join(m.dir(id), "fc.sock"))
	if m.vmmSigue(map[string]int{id: pid}, id, pid) {
		t.Error("sin socket no hay VMM al que hablar")
	}
}
