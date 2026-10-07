package spec

import (
	"path/filepath"
	"testing"
)

func TestParseGraphics(t *testing.T) {
	if g, err := ParseGraphics(""); g != nil || err != nil {
		t.Fatalf("empty: %v %v", g, err)
	}
	g, err := ParseGraphics("720X1280")
	if err != nil || g.Width != 720 || g.Height != 1280 {
		t.Fatalf("720X1280: %+v %v", g, err)
	}
	for _, bad := range []string{"720", "x", "720x", "10x10", "720x99999", "ancho"} {
		if _, err := ParseGraphics(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

// La pantalla tiene que sobrevivir al snapshot: restaurar sin ella (o con otra)
// es una configuración distinta de la guardada.
func TestGraphicsViajaEnElSnapshot(t *testing.T) {
	s := &Spec{
		BootSource:    &BootSource{KernelImagePath: "/k"},
		MachineConfig: &MachineConfig{VCPUCount: 1, MemSizeMiB: 128},
		Drives:        []Drive{{DriveID: "rootfs", PathOnHost: "/r"}},
		Graphics:      &Graphics{Width: 720, Height: 1280},

		MachineIdentifier: "aWQ=",
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "vm.json")
	if err := s.WriteFile(p); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Graphics == nil || *got.Graphics != *s.Graphics {
		t.Fatalf("graphics after the round trip: %+v", got.Graphics)
	}
	if got.KlingVZ != 2 {
		t.Fatalf("a snapshot with a screen is kling_vz %d, want 2: a kling-vz without graphics must refuse it", got.KlingVZ)
	}
	s.Graphics = &Graphics{Width: 1, Height: 1280}
	if err := s.Validate(); err == nil {
		t.Fatal("a 1-pixel-wide screen must not validate")
	}
}
