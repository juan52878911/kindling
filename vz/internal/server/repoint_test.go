package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/vz/internal/spec"
)

// snapshotAjeno escribe un snap.file que apunta a rutas ajenas (lo que haría
// un VMM comprometido) y devuelve su ruta y la del estado.
func snapshotAjeno(t *testing.T) (snap, mem string) {
	t.Helper()
	dir := t.TempDir()
	s := &spec.Spec{
		BootSource:    &spec.BootSource{KernelImagePath: "/etc/passwd", BootArgs: "console=ttyS0 pci=off"},
		MachineConfig: &spec.MachineConfig{VCPUCount: 1, MemSizeMiB: 256},
		Drives: []spec.Drive{
			{DriveID: "rootfs", PathOnHost: "/etc/passwd", IsRootDevice: true, IsReadOnly: true},
			{DriveID: "overlay", PathOnHost: "/etc/shadow"},
		},
		MachineIdentifier: "aWQ=",
	}
	snap, mem = filepath.Join(dir, "snap.file"), filepath.Join(dir, "mem.file")
	if err := s.WriteFile(snap); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mem, []byte("state"), 0o644); err != nil {
		t.Fatal(err)
	}
	return snap, mem
}

func cargaConRepoint(snap, mem, repoint string) string {
	b := `{"snapshot_path":"` + snap + `","mem_backend":{"backend_path":"` + mem + `","backend_type":"File"},"resume_vm":true`
	if repoint != "" {
		b += `,"kling_repoint":` + repoint
	}
	return b + `}`
}

const repointBueno = `{"kernel":"/k/vmlinux","drives":{"rootfs":{"path":"/i/min.ext4","read_only":true},"overlay":{"path":"/m/1/overlay.ext4","read_only":false},"volume":{"path":"/v/x.ext4"}}}`

func TestRepointNingunaRutaDelVolcado(t *testing.T) {
	r := newRig(t)
	var got []Confinamiento
	r.srv.d.Confine = func(c Confinamiento) error { got = append(got, c); return nil }
	snap, mem := snapshotAjeno(t)
	r.must("PUT", "/kling/network", `{"egress":"none"}`)
	r.must("PUT", "/snapshot/load", cargaConRepoint(snap, mem, repointBueno))
	_, sp, _ := r.f.last()
	if sp.BootSource.KernelImagePath != "/k/vmlinux" {
		t.Fatalf("kernel = %q", sp.BootSource.KernelImagePath)
	}
	for _, d := range sp.Drives {
		if strings.HasPrefix(d.PathOnHost, "/etc/") {
			t.Fatalf("Create recibió la ruta del volcado: %+v", d)
		}
	}
	if len(got) != 1 {
		t.Fatalf("Confine llamado %d veces", len(got))
	}
	for _, p := range append(slices.Clone(got[0].Lectura), got[0].Escritura...) {
		if strings.HasPrefix(p, "/etc/") {
			t.Fatalf("Confine recibió la ruta del volcado: %+v", got[0])
		}
	}
	if !slices.Equal(got[0].Escritura, []string{"/m/1/overlay.ext4"}) {
		t.Fatalf("Escritura = %v", got[0].Escritura)
	}
}

func TestRepointFaltaUnDisco(t *testing.T) {
	r := newRig(t)
	confinado := false
	r.srv.d.Confine = func(Confinamiento) error { confinado = true; return nil }
	snap, mem := snapshotAjeno(t)
	r.must("PUT", "/kling/network", `{"egress":"none"}`)
	r.mustFail("PUT", "/snapshot/load", cargaConRepoint(snap, mem,
		`{"kernel":"/k/vmlinux","drives":{"rootfs":{"path":"/i/min.ext4","read_only":true}}}`), "overlay")
	if confinado || len(r.f.vms) != 0 || len(r.nets) != 0 {
		t.Fatalf("no debía confinar, crear ni montar red (confinado=%v vms=%d redes=%d)", confinado, len(r.f.vms), len(r.nets))
	}
	if !strings.Contains(r.must("GET", "/", ""), "Not started") {
		t.Fatal("debía seguir en Not started")
	}
}

func TestRepointFaltaKernel(t *testing.T) {
	r := newRig(t)
	snap, mem := snapshotAjeno(t)
	r.mustFail("PUT", "/snapshot/load", cargaConRepoint(snap, mem,
		`{"drives":{"rootfs":{"path":"/i/min.ext4","read_only":true},"overlay":{"path":"/m/o"}}}`), "kernel")
}

func TestRepointReadOnlyDistinto(t *testing.T) {
	r := newRig(t)
	snap, mem := snapshotAjeno(t)
	r.mustFail("PUT", "/snapshot/load", cargaConRepoint(snap, mem,
		`{"kernel":"/k/vmlinux","drives":{"rootfs":{"path":"/i/min.ext4","read_only":false},"overlay":{"path":"/m/o"}}}`), "rootfs")
}

func TestRepointSinCampoComoHoy(t *testing.T) {
	r := newRig(t)
	snap, mem := snapshotAjeno(t)
	r.must("PUT", "/kling/network", `{"egress":"none"}`)
	r.must("PUT", "/snapshot/load", cargaConRepoint(snap, mem, ""))
	_, sp, _ := r.f.last()
	if sp.BootSource.KernelImagePath != "/etc/passwd" {
		t.Fatalf("sin campo debía usar el volcado, kernel = %q", sp.BootSource.KernelImagePath)
	}
}

func TestInfoAnunciaSnapshotRepoint(t *testing.T) {
	r := newRig(t)
	var info map[string]any
	if err := json.Unmarshal([]byte(r.must("GET", "/kling/info", "")), &info); err != nil {
		t.Fatal(err)
	}
	if info["snapshot_repoint"] != true {
		t.Fatalf("info = %v", info)
	}
}

func TestRepointRechazaEnlaceSimbolico(t *testing.T) {
	r := newRig(t)
	snap, mem := snapshotAjeno(t)
	dir := t.TempDir()
	enlace := filepath.Join(dir, "overlay.ext4")
	if err := os.Symlink("/etc/shadow", enlace); err != nil {
		t.Fatal(err)
	}
	r.must("PUT", "/kling/network", `{"egress":"none"}`)
	r.mustFail("PUT", "/snapshot/load", cargaConRepoint(snap, mem,
		`{"kernel":"/k/vmlinux","drives":{"rootfs":{"path":"/i/min.ext4","read_only":true},"overlay":{"path":"`+enlace+`"}}}`), "symbolic link")
}
