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
		if c.Pending {
			// Aún no existe ni se ha montado: nada garantiza que vaya a poder.
			b.WriteString("store pending (created on first use)")
		} else {
			fmt.Fprintf(&b, "store (reflink inside kindling's %s store)", nombreFSAlmacen(c.Store))
		}
	case "clonefile":
		b.WriteString("clonefile (APFS)")
	default:
		b.WriteString("copy (every instance copies the whole overlay)")
	}
	fmt.Fprintf(&b, "  [daemon.cow=%s]", c.Setting)
	// El motivo, cuando dice algo que el modo no: por qué se copia (espacio,
	// sistema de ficheros, núcleo) o qué almacén se va a crear.
	if c.Reason != "" && (c.Pending || (c.Mode == "copy" && c.Setting != "off")) {
		fmt.Fprintf(&b, "; %s", c.Reason)
	}
	if s := c.Store; s != nil {
		if s.Mounted {
			fmt.Fprintf(&b, "; store %s (%s): %d of %d MiB free", s.Path, nombreFSAlmacen(s), s.FreeMiB, s.SizeMiB)
		} else {
			fmt.Fprintf(&b, "; store %s (%s) NOT mounted", s.Path, nombreFSAlmacen(s))
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

// nombreFSAlmacen es cómo se llama al sistema de ficheros del almacén. Un
// daemon anterior no lo dice, y el suyo solo podía ser XFS.
func nombreFSAlmacen(s *api.CoWStore) string {
	switch {
	case s == nil:
		return "copy-on-write"
	case s.FS == "btrfs":
		return "Btrfs"
	case s.FS == "" || s.FS == "xfs":
		return "XFS"
	}
	return s.FS
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
		d.Detail = "the copy-on-write store " + c.Store.Path + " (" + nombreFSAlmacen(c.Store) + ") exists but is not mounted: instances with their overlay there won't start"
		d.Fix = "restart the daemon (it mounts the store), and check its log for the mount error"
	case c.Store != nil && c.Store.NoQuota:
		d.State = doctorWarn
		d.Detail = "the copy-on-write store " + c.Store.Path + " (" + nombreFSAlmacen(c.Store) + ") has no per-instance disk quota: a compromised VMM could grow its overlay and fill the store"
		d.Fix = "install xfs_quota (xfsprogs) or btrfs (btrfs-progs); an XFS store mounted without prjquota needs the daemon stopped, no microVMs running and `umount " + c.Store.Path + "` so the daemon remounts it with quota (docs/cow.md)"
	case c.Mode == "copy" && c.Setting != "off":
		d.State = doctorWarn
		d.Detail = "run -from copies the whole golden overlay: " + c.Reason
		d.Fix = "install xfsprogs on the daemon host (btrfs-progs if its kernel has no XFS, e.g. inside a Proxmox LXC) and restart it (docs/cow.md), or `kling config set daemon.cow off` to silence this"
	}
	return d, true
}
