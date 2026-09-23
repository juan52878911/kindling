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
	if !fatales(viejo)["macOS 14+"] {
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
	sinE2fs := sano
	sinE2fs.debugfs = false
	if !fatales(sinE2fs)["e2fsprogs"] {
		t.Fatal("sin debugfs")
	}
	// Sin imágenes el daemon arranca igual: es un aviso, no un fatal.
	sinImg := sano
	sinImg.kernel, sinImg.baseImage = false, false
	if f := fatales(sinImg); len(f) != 0 {
		t.Fatalf("sin imágenes no es fatal: %v", f)
	}
	if versionMayor("14.0") != 14 || versionMayor("") != 0 || versionMayor("x") != 0 {
		t.Fatal("versionMayor")
	}
}
