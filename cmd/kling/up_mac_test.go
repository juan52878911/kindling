package main

import "testing"

func TestChecksMac(t *testing.T) {
	sano := probeMac{arch: "arm64", macOS: "15.5", vmm: "/usr/local/bin/kling-vz", vmmVersion: "kling-vz 0.1.0",
		entitlement: true, mkfs: true, debugfs: true, kernel: true, baseImage: true, root: "/r"}
	for _, c := range checksMac(sano) {
		if !c.ok {
			t.Fatalf("un Mac sano falla %q", c.label)
		}
	}
	fatales := func(p probeMac) map[string]bool {
		out := map[string]bool{}
		for _, c := range checksMac(p) {
			if !c.ok && c.fatal {
				out[c.label] = true
			}
		}
		return out
	}
	viejo := sano
	viejo.macOS = "13.6"
	if !fatales(viejo)["macOS 14.0+"] {
		t.Fatal("macOS 13 no vale")
	}
	intel := sano
	intel.arch = "amd64"
	if !fatales(intel)["Apple Silicon"] {
		t.Fatal("Intel no vale")
	}
	sinVZ := sano
	sinVZ.vmm, sinVZ.vmmVersion, sinVZ.entitlement = "", "", false
	if f := fatales(sinVZ); !f["kling-vz"] || !f["vz entitlement"] {
		t.Fatalf("sin kling-vz: %v", f)
	}
	sinFirma := sano
	sinFirma.entitlement = false
	if f := fatales(sinFirma); !f["vz entitlement"] || f["kling-vz"] {
		t.Fatalf("sin firma: %v", f)
	}
	sinMkfs := sano
	sinMkfs.mkfs = false
	if !fatales(sinMkfs)["mkfs.ext4"] {
		t.Fatal("sin mkfs.ext4 no hay overlay: es fatal")
	}
	// Sin debugfs se arranca igual (solo falla inspeccionar imágenes): aviso.
	sinDebugfs := sano
	sinDebugfs.debugfs = false
	if f := fatales(sinDebugfs); len(f) != 0 {
		t.Fatalf("sin debugfs no es fatal: %v", f)
	}
	// Sin imágenes el daemon arranca igual: es un aviso, no un fatal.
	sinImg := sano
	sinImg.kernel, sinImg.baseImage = false, false
	if f := fatales(sinImg); len(f) != 0 {
		t.Fatalf("sin imágenes no es fatal: %v", f)
	}
	for v, want := range map[string]bool{"14.0": true, "14": true, "13.6.1": false, "26.5": true, "": false, "x": false, "14.x": false} {
		if versionAlMenos(v, "14.0") != want {
			t.Fatalf("versionAlMenos(%q, 14.0) != %v", v, want)
		}
	}
	for v, want := range map[string]bool{"15.1": false, "15.1.1": false, "15.2": true, "15.2.0": true, "15.10": true, "16.0": true, "15": false} {
		if versionAlMenos(v, "15.2") != want {
			t.Fatalf("versionAlMenos(%q, 15.2) != %v", v, want)
		}
	}
}

// Con daemon.vmm hv se mira kling-hv, su entitlement y macOS 15 (el GIC de
// Hypervisor.framework).
func TestChecksMacHV(t *testing.T) {
	p := probeMac{backend: "hv", arch: "arm64", macOS: "14.6", vmm: "/usr/local/bin/kling-hv", vmmVersion: "kling-hv 0.1.0",
		entitlement: true, mkfs: true, debugfs: true, kernel: true, baseImage: true, root: "/r"}
	vistos := map[string]bool{}
	for _, c := range checksMac(p) {
		vistos[c.label] = true
		if c.label == "macOS 15.2+" && c.ok {
			t.Fatal("kling-hv no vale en macOS 14")
		}
	}
	for _, l := range []string{"kling-hv", "hv entitlement", "macOS 15.2+"} {
		if !vistos[l] {
			t.Fatalf("falta el check %q: %v", l, vistos)
		}
	}
	// 15.1 tiene el mismo mayor pero no el mínimo de kling-hv.
	for v, ok := range map[string]bool{"15.1": false, "15.2": true, "26.5": true} {
		p.macOS = v
		for _, c := range checksMac(p) {
			if c.label == "macOS 15.2+" && c.ok != ok {
				t.Fatalf("macOS %s con hv: ok=%v, want %v", v, c.ok, ok)
			}
		}
	}
}
