//go:build darwin && arm64

package hvvm

import (
	"io"
	"strings"
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
