package main

import (
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestLineaCoW(t *testing.T) {
	if lineaCoW(nil) != "" {
		t.Error("un daemon anterior no dice nada")
	}
	l := lineaCoW(&api.CoWInfo{Setting: "auto", Mode: "store",
		Store:  &api.CoWStore{Path: "/var/lib/kindling/cow", Mounted: true, SizeMiB: 16384, FreeMiB: 16000},
		Clones: map[string]int64{"store": 32, "copy": 1}})
	for _, w := range []string{"XFS store", "daemon.cow=auto", "16000 of 16384 MiB free", "copy 1, store 32"} {
		if !strings.Contains(l, w) {
			t.Errorf("falta %q en %q", w, l)
		}
	}
	// Un almacén Btrfs lo dice, en el modo y en el estado del almacén.
	l = lineaCoW(&api.CoWInfo{Setting: "auto", Mode: "store",
		Store: &api.CoWStore{Path: "/var/lib/kindling/cow", FS: "btrfs", Mounted: true, SizeMiB: 4096, FreeMiB: 4000}})
	for _, w := range []string{"Btrfs store", "cow (Btrfs): 4000 of 4096 MiB free"} {
		if !strings.Contains(l, w) {
			t.Errorf("falta %q en %q", w, l)
		}
	}
	if !strings.HasPrefix(l, "store ") {
		t.Errorf("la primera palabra es el modo (bench-cow.sh): %q", l)
	}
}

func TestCheckCoW(t *testing.T) {
	if _, ok := checkCoW(nil); ok {
		t.Error("sin información no hay comprobación")
	}
	c, _ := checkCoW(&api.CoWInfo{Setting: "auto", Mode: "reflink"})
	if c.State != doctorOK {
		t.Errorf("reflink: %+v", c)
	}
	c, _ = checkCoW(&api.CoWInfo{Setting: "auto", Mode: "copy", Reason: "mkfs.xfs not found"})
	if c.State != doctorWarn || !strings.Contains(c.Detail, "mkfs.xfs") || c.Fix == "" {
		t.Errorf("copia sin pedirla: %+v", c)
	}
	c, _ = checkCoW(&api.CoWInfo{Setting: "off", Mode: "copy"})
	if c.State != doctorOK {
		t.Errorf("off es una decisión: %+v", c)
	}
	c, _ = checkCoW(&api.CoWInfo{Setting: "off", Mode: "copy", Store: &api.CoWStore{Path: "/x/cow"}})
	if c.State != doctorWarn || !strings.Contains(c.Detail, "not mounted") {
		t.Errorf("almacén sin montar: %+v", c)
	}
	c, _ = checkCoW(&api.CoWInfo{Setting: "auto", Mode: "store", Store: &api.CoWStore{Path: "/x/cow", FS: "xfs", Mounted: true, NoQuota: true}})
	if c.State != doctorWarn || !strings.Contains(c.Detail, "quota") || c.Fix == "" {
		t.Errorf("almacén sin cuota: %+v", c)
	}
	c, _ = checkCoW(&api.CoWInfo{Setting: "auto", Mode: "store", Store: &api.CoWStore{Path: "/x/cow", FS: "xfs", Mounted: true, Quota: "prjquota"}})
	if c.State != doctorOK {
		t.Errorf("almacén con cuota: %+v", c)
	}
}
