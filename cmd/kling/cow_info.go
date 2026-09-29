package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
)

// lineaCoW resume para `kling info` cómo reciben su disco las instancias de
// un dorado (daemon.cow, docs/cow.md). "" con un daemon que no lo dice.
func lineaCoW(c *api.CoWInfo) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	// La primera palabra es el modo tal cual (scripts/bench-cow.sh la lee).
	switch c.Mode {
	case "reflink":
		b.WriteString("reflink (the data root shares blocks)")
	case "store":
		b.WriteString("store (reflink inside kindling's XFS store)")
	case "clonefile":
		b.WriteString("clonefile (APFS)")
	default:
		b.WriteString("copy (every instance copies the whole overlay)")
	}
	fmt.Fprintf(&b, "  [daemon.cow=%s]", c.Setting)
	if s := c.Store; s != nil {
		if s.Mounted {
			fmt.Fprintf(&b, "; store %s: %d of %d MiB free", s.Path, s.FreeMiB, s.SizeMiB)
		} else {
			fmt.Fprintf(&b, "; store %s NOT mounted", s.Path)
		}
	}
	if len(c.Clones) > 0 {
		modos := make([]string, 0, len(c.Clones))
		for k := range c.Clones {
			modos = append(modos, k)
		}
		sort.Strings(modos)
		partes := make([]string, len(modos))
		for i, k := range modos {
			partes[i] = fmt.Sprintf("%s %d", k, c.Clones[k])
		}
		fmt.Fprintf(&b, "; since start: %s", strings.Join(partes, ", "))
	}
	return b.String()
}

// checkCoW es la comprobación de `kling doctor` del modo de copias: aviso si
// las instancias copian el disco entero sin que nadie lo haya pedido, o si
// hay un almacén sin montar (sus instancias no arrancan).
func checkCoW(c *api.CoWInfo) (doctorCheck, bool) {
	if c == nil {
		return doctorCheck{}, false
	}
	d := doctorCheck{Name: "disk clones", State: doctorOK, Detail: lineaCoW(c)}
	switch {
	case c.Store != nil && !c.Store.Mounted:
		d.State = doctorWarn
		d.Detail = "the copy-on-write store " + c.Store.Path + " exists but is not mounted: instances with their overlay there won't start"
		d.Fix = "restart the daemon (it mounts the store), and check its log for the mount error"
	case c.Mode == "copy" && c.Setting != "off":
		d.State = doctorWarn
		d.Detail = "run -from copies the whole golden overlay: " + c.Reason
		d.Fix = "install xfsprogs on the daemon host and restart it (docs/cow.md), or `kling config set daemon.cow off` to silence this"
	}
	return d, true
}
