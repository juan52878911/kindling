package spec

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTranslateBootArgs(t *testing.T) {
	cases := []struct{ in, want string }{
		{
			"console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=/sbin/overlay-init ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off",
			"console=hvc0 reboot=k panic=1 root=/dev/vda ro init=/sbin/overlay-init ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off",
		},
		{"console=ttyS0,115200 kling.layer=/dev/vdc", "console=hvc0 kling.layer=/dev/vdc"},
		{"  pci=off   root=/dev/vda  ", "root=/dev/vda"},
		// Solo se tocan las palabras exactas: un parámetro que las contenga pasa.
		{"kling.note=pci=off console=ttyS1", "kling.note=pci=off console=ttyS1"},
		{"", ""},
	}
	for _, c := range cases {
		if got := TranslateBootArgs(c.in); got != c.want {
			t.Errorf("TranslateBootArgs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func sample() *Spec {
	s := &Spec{
		BootSource:        &BootSource{KernelImagePath: "/k/vmlinux", BootArgs: "console=ttyS0 pci=off"},
		MachineConfig:     &MachineConfig{VCPUCount: 1, MemSizeMiB: 256},
		Network:           &NetworkInterface{IfaceID: "eth0", HostDevName: "tap0", GuestMAC: "06:00:AC:10:00:02"},
		MMDSConfig:        &MMDSConfig{Version: "V2", IPv4Address: "169.254.169.254", NetworkInterfaces: []string{"eth0"}},
		Balloon:           &Balloon{AmountMiB: 64, DeflateOnOOM: true, StatsPollingIntervalS: 1},
		Entropy:           true,
		MachineIdentifier: "aWQ=",
	}
	s.PutDrive(Drive{DriveID: "rootfs", PathOnHost: "/i/min.ext4", IsRootDevice: true, IsReadOnly: true})
	s.PutDrive(Drive{DriveID: "overlay", PathOnHost: "/m/overlay.ext4"})
	s.PutDrive(Drive{DriveID: "layer", PathOnHost: "/i/x.layer.ext4", IsReadOnly: true})
	return s
}

func TestPutDriveKeepsOrder(t *testing.T) {
	s := sample()
	// Reconfigurar el primero no debe moverlo al final: el orden son las letras.
	s.PutDrive(Drive{DriveID: "rootfs", PathOnHost: "/i/other.ext4", IsReadOnly: true})
	var ids []string
	for _, d := range s.Drives {
		ids = append(ids, d.DriveID)
	}
	if !reflect.DeepEqual(ids, []string{"rootfs", "overlay", "layer"}) {
		t.Fatalf("order = %v", ids)
	}
	if s.Drives[0].PathOnHost != "/i/other.ext4" {
		t.Fatalf("drive not replaced: %+v", s.Drives[0])
	}
	if err := s.PatchDrivePath("nope", "/x"); err == nil {
		t.Fatal("patching an unknown drive must fail")
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	s := sample()
	path := filepath.Join(t.TempDir(), "snap.json")
	if err := s.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := s.Clone()
	want.KlingVZ = FormatVersion
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	// El JSON lleva los nombres del contrato.
	b, _ := os.ReadFile(path)
	for _, k := range []string{`"kling_vz": 1`, `"boot_source"`, `"machine_config"`, `"drives"`, `"network"`,
		`"mmds_config"`, `"balloon"`, `"entropy"`, `"machine_identifier"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("snapshot JSON lacks %s", k)
		}
	}
	// Escribir no deja temporales al lado.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("unexpected files next to the snapshot: %v", entries)
	}
}

func TestDecodeRejects(t *testing.T) {
	cases := map[string]string{
		"not json":        "firecracker binary snapshot",
		"wrong version":   `{"kling_vz": 2, "machine_identifier": "x"}`,
		"no identifier":   `{"kling_vz": 1, "boot_source": {"kernel_image_path": "/k"}, "machine_config": {"vcpu_count": 1, "mem_size_mib": 128}, "drives": [{"drive_id": "r", "path_on_host": "/r"}]}`,
		"incomplete":      `{"kling_vz": 1, "machine_identifier": "x"}`,
		"too large":       `{"kling_vz": 1, "pad": "` + strings.Repeat("a", maxSnapshotJSON) + `"}`,
		"balloon too big": `{"kling_vz": 1, "machine_identifier": "x", "boot_source": {"kernel_image_path": "/k"}, "machine_config": {"vcpu_count": 1, "mem_size_mib": 128}, "drives": [{"drive_id": "r", "path_on_host": "/r"}], "balloon": {"amount_mib": 128}}`,
	}
	for name, in := range cases {
		if _, err := Decode(strings.NewReader(in)); err == nil {
			t.Errorf("%s: Decode accepted it", name)
		}
	}
}

func TestBalloonTarget(t *testing.T) {
	s := sample()
	if got := s.BalloonTargetMiB(); got != 192 {
		t.Fatalf("target = %d, want 192 (256 - 64 inflated)", got)
	}
	s.Balloon = nil
	if got := s.BalloonTargetMiB(); got != 256 {
		t.Fatalf("target without balloon = %d", got)
	}
}
