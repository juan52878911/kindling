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
	if strings.Contains(l, "no reflink") {
		t.Errorf("con el almacén montado no hace falta el motivo: %q", l)
	}
}

// #60: antes del primer run -from el almacén no existe; no se dice que se
// clona dentro de él, sino que está pendiente, y qué se va a crear.
func TestLineaCoWPendiente(t *testing.T) {
	l := lineaCoW(&api.CoWInfo{Setting: "auto", Mode: "store", Pending: true,
		Reason: "no reflink on the data root: overlays are reflinked inside kindling's copy-on-write store (xfs store, created on the first run -from)"})
	for _, w := range []string{"store pending (created on first use)", "daemon.cow=auto", "xfs store, created on the first run -from"} {
		if !strings.Contains(l, w) {
			t.Errorf("falta %q en %q", w, l)
		}
	}
	if strings.Contains(l, "reflink inside kindling's XFS store)") {
		t.Errorf("da el almacén por hecho: %q", l)
	}
	if !strings.HasPrefix(l, "store ") {
		t.Errorf("la primera palabra es el modo (bench-cow.sh): %q", l)
	}
	// Si no se puede, el motivo sale en la línea: espacio, sistema de
	// ficheros o núcleo.
	l = lineaCoW(&api.CoWInfo{Setting: "auto", Mode: "copy",
		Reason: "no reflink on the data root and no copy-on-write store (only 3000 MiB free under the data root: not enough for a store (needs 4 GiB free)): copying overlays"})
	if !strings.HasPrefix(l, "copy ") || !strings.Contains(l, "only 3000 MiB free") {
		t.Errorf("la copia no dice por qué: %q", l)
	}
	// Con daemon.cow=off no hay nada que explicar.
	l = lineaCoW(&api.CoWInfo{Setting: "off", Mode: "copy", Reason: "daemon.cow is off"})
	if strings.Contains(l, "daemon.cow is off;") || strings.Contains(l, "; daemon.cow is off") {
		t.Errorf("repite lo configurado: %q", l)
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
