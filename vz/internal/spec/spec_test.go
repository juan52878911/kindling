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
	want.KlingVZ = 1 // sin pantalla: lo lee también un kling-vz anterior
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
		"wrong version":   `{"kling_vz": 0, "machine_identifier": "x"}`,
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

// graphics entró sin subir kling_vz, y un kling-vz anterior restauraba sin
// pantalla un snapshot que la tenía. Ahora se escribe la 2; la 1 se sigue
// leyendo (no tiene graphics), y una del futuro se niega diciendo qué hacer.
func TestSnapshotVersion(t *testing.T) {
	s := sample()
	s.Graphics = &Graphics{Width: 720, Height: 1280}
	path := filepath.Join(t.TempDir(), "snap.json")
	if err := s.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"kling_vz": 2`) {
		t.Fatalf("snapshot con graphics escrito sin kling_vz 2:\n%s", b)
	}

	v1 := strings.Replace(string(b), `"kling_vz": 2`, `"kling_vz": 1`, 1)
	if _, err := Decode(strings.NewReader(v1)); err != nil {
		t.Fatalf("un snapshot de la versión 1 se tiene que seguir leyendo: %v", err)
	}

	futuro := strings.Replace(string(b), `"kling_vz": 2`, `"kling_vz": 99`, 1)
	_, err := Decode(strings.NewReader(futuro))
	if err == nil || !strings.Contains(err.Error(), "newer kling-vz") || !strings.Contains(err.Error(), "update kling-vz") {
		t.Fatalf("snapshot del futuro: %v", err)
	}
}

// Un snapshot sin nada de la versión 2 (sin pantalla) se escribe como 1: el
// kling-vz anterior lo restaura igual, y es lo que hace falta para volver atrás
// (kling upgrade -rollback) con máquinas que el nuevo congeló. Con pantalla, 2.
func TestSnapshotEscribeLaVersionMasBaja(t *testing.T) {
	for _, c := range []struct {
		g    *Graphics
		want string
	}{{nil, `"kling_vz": 1`}, {&Graphics{Width: 720, Height: 1280}, `"kling_vz": 2`}} {
		s := sample()
		s.Graphics = c.g
		path := filepath.Join(t.TempDir(), "snap.json")
		if err := s.WriteFile(path); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(path); !strings.Contains(string(b), c.want) {
			t.Errorf("graphics %v: want %s in\n%s", c.g, c.want, b)
		}
	}
}

// Lo que dice -snapshot-formats: lee de la 1 a la actual.
func TestFormatos(t *testing.T) {
	if min, max := Formatos(); min != 1 || max != FormatVersion {
		t.Fatalf("Formatos() = %d %d", min, max)
	}
}

// Desechable es de la máquina, no del snapshot: no se escribe, y un PATCH lo
// cambia sin tocar los demás campos.
func TestDesechableNoVaAlSnapshot(t *testing.T) {
	s := sample()
	s.PutDrive(Drive{DriveID: "overlay", PathOnHost: "/m/overlay.ext4", Disposable: true})
	if c := s.Clone(); !c.Drives[1].Disposable {
		t.Fatal("Clone pierde kling_disposable (Create recibe un Clone)")
	}
	path := filepath.Join(t.TempDir(), "snap.json")
	if err := s.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "kling_disposable") {
		t.Fatalf("el snapshot lleva kling_disposable:\n%s", b)
	}
	if !s.Drives[1].Disposable {
		t.Fatal("WriteFile tocó la spec en memoria")
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	si := true
	if err := got.PatchDrive("overlay", "/m2/overlay.ext4", &si); err != nil {
		t.Fatal(err)
	}
	d := got.Drives[1]
	if d.PathOnHost != "/m2/overlay.ext4" || !d.SyncNone() {
		t.Fatalf("tras PATCH: %+v", d)
	}
	if (Drive{Disposable: true, IsReadOnly: true}).SyncNone() || (Drive{Disposable: true, IsRootDevice: true}).SyncNone() {
		t.Fatal("SyncNone en un disco de solo lectura o en la raíz")
	}
}

// vsock y una MTU distinta de 1500 cambian la máquina restaurada: se escriben
// como 3, que un kling-vz anterior rechaza en vez de restaurar sin ellos. Una
// MTU de 1500 explícita es la de siempre y no sube nada.
func TestSnapshotVersion3(t *testing.T) {
	for _, c := range []struct {
		vsock bool
		mtu   int
		want  string
	}{{true, 0, `"kling_vz": 3`}, {false, 16384, `"kling_vz": 3`}, {false, 1500, `"kling_vz": 1`}, {false, 0, `"kling_vz": 1`}} {
		s := sample()
		s.Vsock, s.MTU = c.vsock, c.mtu
		path := filepath.Join(t.TempDir(), "snap.json")
		if err := s.WriteFile(path); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(path)
		if !strings.Contains(string(b), c.want) {
			t.Errorf("vsock=%v mtu=%d: want %s in\n%s", c.vsock, c.mtu, c.want, b)
		}
		got, err := Decode(strings.NewReader(string(b)))
		if err != nil || got.Vsock != c.vsock || got.MTU != c.mtu {
			t.Errorf("vsock=%v mtu=%d: releído %+v, %v", c.vsock, c.mtu, got, err)
		}
	}
}

func TestValidateMTU(t *testing.T) {
	for mtu, ok := range map[int]bool{0: true, 1500: true, 16384: true, 65535: true, 1499: false, 65536: false} {
		s := sample()
		s.MTU = mtu
		if err := s.Validate(); (err == nil) != ok {
			t.Errorf("mtu %d: %v", mtu, err)
		}
	}
}

func TestParseMTUYVsock(t *testing.T) {
	for v, want := range map[string]int{"": DefaultMTU, "0": 0, "1500": 0, "9000": 9000, "65535": 65535} {
		if got, err := ParseMTU(v); err != nil || got != want {
			t.Errorf("ParseMTU(%q) = %d, %v; want %d", v, got, err, want)
		}
	}
	for _, v := range []string{"1499", "65536", "x", "-1"} {
		if _, err := ParseMTU(v); err == nil {
			t.Errorf("ParseMTU(%q) debía fallar", v)
		}
	}
	for v, want := range map[string]bool{"": true, "1": true, "0": false} {
		if got, err := ParseVsock(v); err != nil || got != want {
			t.Errorf("ParseVsock(%q) = %v, %v", v, got, err)
		}
	}
	if _, err := ParseVsock("si"); err == nil {
		t.Error("ParseVsock(si) debía fallar")
	}
}
