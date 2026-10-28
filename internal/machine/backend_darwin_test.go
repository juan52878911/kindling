//go:build darwin

package machine

import "testing"

// hv es la plataforma de macOS con la memoria compartida entre copias (como
// Firecracker) y más restauraciones a la vez; vz, la de siempre. El congelado
// diferencial sigue siendo solo de Firecracker.
func TestElegirBackendMac(t *testing.T) {
	t.Cleanup(func() { _ = ElegirBackend(BackendVZ) })
	if err := ElegirBackend(BackendHV); err != nil {
		t.Fatal(err)
	}
	if backendVMM != BackendHV || !restaurarComparteMemoria || lanzamientoPlataforma() != restoresSimultaneosHV {
		t.Fatalf("hv: backend %q, comparte %v, restauraciones %d", backendVMM, restaurarComparteMemoria, lanzamientoPlataforma())
	}
	if congelarEnDiff() {
		t.Fatal("hv no congela en diferencial")
	}
	if NombreVMM(BackendHV) != "kling-hv" {
		t.Fatal(NombreVMM(BackendHV))
	}
	if err := ElegirBackend(BackendVZ); err != nil {
		t.Fatal(err)
	}
	if backendVMM != BackendVZ || restaurarComparteMemoria || lanzamientoPlataforma() != restoresSimultaneosVZ {
		t.Fatalf("vz: backend %q, comparte %v", backendVMM, restaurarComparteMemoria)
	}
	if ElegirBackend(BackendFirecracker) == nil || BackendSoportado(BackendFirecracker) {
		t.Fatal("firecracker no se habla en macOS")
	}
	if !BackendSoportado(BackendHV) || !BackendSoportado(BackendVZ) {
		t.Fatal("vz y hv se hablan en macOS")
	}
}
