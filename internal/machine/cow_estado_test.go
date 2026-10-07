package machine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// #60: en modo store, hasta el primer run -from el almacén no existe y
// CoWInfo lo dice (Pending); después ya no.
func TestCoWInfoPendienteHastaElPrimerUso(t *testing.T) {
	m := newTestManager(t)
	m.alm = nuevoAlmacenFalso(t, m.root, &almacenFalso{})
	m.cow.pedido, m.cow.modo, m.cow.motivo = CoWAuto, cowModoStore, "no reflink"+notaAlmacenPendiente("btrfs", true)
	info := m.CoWInfo()
	if !info.Pending || info.Store != nil {
		t.Fatalf("antes de usarlo: %+v", info)
	}
	if !strings.Contains(info.Reason, "btrfs store, created on the first save or run -from") {
		t.Errorf("motivo: %q", info.Reason)
	}
	src := escribirDorado(t, m.root, "d", "x")
	dst := filepath.Join(m.root, "overlay.ext4")
	if modo, err := m.clonarOverlayInstancia(context.Background(), "d", src, "id1", dst); err != nil || modo != cowModoStore {
		t.Fatalf("modo=%q err=%v", modo, err)
	}
	if info := m.CoWInfo(); info.Pending || info.Store == nil || !info.Store.Mounted {
		t.Errorf("tras el primer uso: %+v", info)
	}
	// En otro modo nunca está pendiente.
	m2 := newTestManager(t)
	m2.alm = nuevoAlmacenFalso(t, m2.root, &almacenFalso{})
	m2.cow.modo = cowModoCopy
	if m2.CoWInfo().Pending {
		t.Error("pendiente en modo copy")
	}
}

func TestNotaAlmacenPendiente(t *testing.T) {
	if n := notaAlmacenPendiente("xfs", true); strings.Contains(n, "kernel") || !strings.Contains(n, "xfs store") {
		t.Errorf("con el módulo cargado: %q", n)
	}
	if n := notaAlmacenPendiente("xfs", false); !strings.Contains(n, "the kernel does not list xfs yet") {
		t.Errorf("sin el módulo: %q", n)
	}
}

// Sin sitio para un almacén se sabe al arrancar, con el motivo.
func TestComprobarEspacioAlmacen(t *testing.T) {
	root := t.TempDir()
	a := nuevoAlmacenFalso(t, root, &almacenFalso{libre: 3 << 30})
	if err := a.comprobarEspacio(0); err == nil || !strings.Contains(err.Error(), "MiB free") {
		t.Errorf("3 GiB libres: %v", err)
	}
	a = nuevoAlmacenFalso(t, root, &almacenFalso{})
	if err := a.comprobarEspacio(0); err != nil {
		t.Errorf("100 GiB libres: %v", err)
	}
	modo, motivo := decidirCoW(CoWAuto, false, a.comprobarEspacio(200))
	if modo != cowModoCopy || !strings.Contains(motivo, "doesn't fit") {
		t.Errorf("daemon.cow_store_gib que no cabe: %q %q", modo, motivo)
	}
}
