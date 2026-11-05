//go:build darwin && arm64

package hvvm

import (
	"io"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/juan52878911/kindling/vz/internal/spec"
)

func specMinimo(drives int) *spec.Spec {
	s := &spec.Spec{
		BootSource:    &spec.BootSource{KernelImagePath: "/nonexistent/vmlinux", BootArgs: "console=ttyS0"},
		MachineConfig: &spec.MachineConfig{VCPUCount: 1, MemSizeMiB: 128},
	}
	for i := 0; i < drives; i++ {
		s.Drives = append(s.Drives, spec.Drive{DriveID: "d", PathOnHost: "/nonexistent/disk", IsReadOnly: true})
	}
	return s
}

// Lo que se rechaza antes de tocar Hypervisor.framework.
func TestCreateRechazaLoQueNoTiene(t *testing.T) {
	f := &Factory{Console: io.Discard, RAMDir: t.TempDir()}
	if _, err := f.Create(specMinimo(9), "", nil); err == nil || !strings.Contains(err.Error(), "at most 8 drives") {
		t.Fatalf("9 drives: %v", err)
	}
	s := specMinimo(1)
	s.Graphics = &spec.Graphics{Width: 640, Height: 480}
	if _, err := f.Create(s, "", nil); err == nil || !strings.Contains(err.Error(), "display") {
		t.Fatalf("graphics: %v", err)
	}
	// Más vCPU de los que deja Hypervisor.framework (o MAX_CPUS): khv_create
	// lo rechaza antes de crear la VM.
	s = specMinimo(1)
	s.MachineConfig.VCPUCount = maxVCPUsC() + 1
	if _, err := f.Create(s, "", nil); err == nil || !strings.Contains(err.Error(), "vcpu_count must be between 1 and") {
		t.Fatalf("%d vCPU: %v", s.MachineConfig.VCPUCount, err)
	}
}

// MaxVCPUs: lo que deja el framework sin pasar de los núcleos del host.
func TestMaxVCPUs(t *testing.T) {
	m := MaxVCPUs()
	if m < 1 || m > runtime.NumCPU() || m > maxVCPUsC() {
		t.Fatalf("MaxVCPUs() = %d (host %d cores, khv_max_vcpus %d)", m, runtime.NumCPU(), maxVCPUsC())
	}
}

// Un binario de pruebas no lleva el entitlement com.apple.security.hypervisor:
// crear la VM falla con el error del framework, no con un cuelgue ni un exit.
func TestCreateSinEntitlement(t *testing.T) {
	f := &Factory{Console: io.Discard, RAMDir: t.TempDir()}
	_, err := f.Create(specMinimo(1), "", nil)
	if err == nil {
		t.Skip("this test binary may create VMs (signed with the entitlement?)")
	}
	if !strings.Contains(err.Error(), "hv_vm_create") && !strings.Contains(err.Error(), "disk") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewMachineID(t *testing.T) {
	f := &Factory{}
	a, err := f.NewMachineID()
	if err != nil || len(a) != 24 {
		t.Fatalf("%q %v", a, err)
	}
	if b, _ := f.NewMachineID(); a == b {
		t.Fatal("two equal machine ids")
	}
}

// Un dial cuyo slot se libera mientras espera (el RST del invitado) y que otro
// dial toma antes de que despierte: el primero no puede quedarse con la
// conexión del segundo (que reconocía solo por el número de fd) ni mandarle
// un RST.
// Una conexión que el invitado dejó de leer y Go cerró del todo tiene que
// pedirle el cierre completo (y con él su RST, o el plazo de 2 s): antes no
// entraba en el poll y el slot y su fd se quedaban para siempre. Un medio
// cierre de Go (CloseWrite) no debe cerrarla.
func TestVsockCierreDelHost(t *testing.T) {
	if got := vsockCierre(false); got != 1 {
		t.Fatalf("Go cerró del todo: cierre completo pedido = %d, quiero 1", got)
	}
	if got := vsockCierre(true); got != 0 {
		t.Fatalf("Go solo cerró su escritura: cierre completo pedido = %d, quiero 0", got)
	}
}

func TestVsockDialSlotReusado(t *testing.T) {
	for i := 0; i < 20; i++ {
		ret, mismo := vsockReuso()
		if ret != -int(syscall.ECONNREFUSED) {
			t.Fatalf("vuelta %d: el primer dial dio %d (mismo fd: %v), quiero -ECONNREFUSED", i, ret, mismo)
		}
		if i == 0 {
			t.Logf("mismo número de fd en el slot reusado: %v", mismo)
		}
	}
}
