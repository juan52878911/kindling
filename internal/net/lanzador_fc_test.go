//go:build linux

package net

import (
	"errors"
	"testing"
)

// fakeNetns sustituye las llamadas del lanzador y apunta a qué netns está el
// hilo. fallaVuelta hace fallar el setns de regreso.
func fakeNetns(t *testing.T, fallaVuelta bool) *[]string {
	t.Helper()
	old := opsNetns
	t.Cleanup(func() { opsNetns = old })
	var traza []string
	opsNetns.abrir = func(ruta string) (int, error) {
		traza = append(traza, "abrir "+ruta)
		if ruta == "/proc/thread-self/ns/net" {
			return 10, nil
		}
		return 11, nil
	}
	opsNetns.setns = func(fd int) error {
		if fd == 10 && fallaVuelta {
			return errors.New("EPERM")
		}
		traza = append(traza, "setns")
		return nil
	}
	opsNetns.cerrar = func(int) {}
	return &traza
}

func TestLanzadorDevuelveElHiloSiRestauraElNetns(t *testing.T) {
	fakeNetns(t, false)
	antes := hilosPerdidos.Load()
	devuelto, err := enHiloDeNetns("kl-x", func() error { return nil })
	if err != nil || !devuelto || hilosPerdidos.Load() != antes {
		t.Fatalf("devuelto=%v err=%v perdidos=%d", devuelto, err, hilosPerdidos.Load()-antes)
	}
}

func TestLanzadorNoDevuelveElHiloSiNoPuedeVolver(t *testing.T) {
	fakeNetns(t, true)
	antes := hilosPerdidos.Load()
	llamado := false
	devuelto, err := enHiloDeNetns("kl-x", func() error { llamado = true; return nil })
	if !llamado {
		t.Fatal("f no se ejecutó")
	}
	if devuelto || err == nil || hilosPerdidos.Load() != antes+1 {
		t.Fatalf("devuelto=%v err=%v perdidos=%d: el hilo que no volvió no debe devolverse", devuelto, err, hilosPerdidos.Load()-antes)
	}
}

func TestLanzadorSiElSetnsDeIdaFallaElHiloSigueEnSuNetns(t *testing.T) {
	old := opsNetns
	t.Cleanup(func() { opsNetns = old })
	opsNetns.abrir = func(string) (int, error) { return 3, nil }
	opsNetns.cerrar = func(int) {}
	opsNetns.setns = func(int) error { return errors.New("EINVAL") }
	devuelto, err := enHiloDeNetns("kl-x", func() error { t.Fatal("f no debe correr fuera del netns"); return nil })
	if err == nil || !devuelto {
		t.Fatalf("devuelto=%v err=%v", devuelto, err)
	}
}
