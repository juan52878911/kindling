//go:build !darwin

package net

import (
	"strings"
	"testing"
)

// grabarSetup monta una red con todo lo que lanza procesos sustituido y
// devuelve, en orden, una línea por proceso que Setup habría lanzado.
func grabarSetup(t *testing.T, egress Egress) []string {
	t.Helper()
	oldE, oldB, oldR, oldH, oldX, oldI, oldA := ejecutar, ipBatch, ejecutarRestore, hayRestore, existeRed, hayIp6tables, asegurarHost
	t.Cleanup(func() {
		ejecutar, ipBatch, ejecutarRestore, hayRestore, existeRed, hayIp6tables, asegurarHost = oldE, oldB, oldR, oldH, oldX, oldI, oldA
	})
	var procs []string
	ejecutar = func(args ...string) error { procs = append(procs, strings.Join(args, " ")); return nil }
	ipBatch = func(ns string, o ...string) error { procs = append(procs, "ip-batch "+ns); return nil }
	ejecutarRestore = func(ns, _ string) ([]byte, error) { procs = append(procs, "iptables-restore "+ns); return nil, nil }
	hayRestore = func() bool { return true }
	hayIp6tables = func() bool { return true }
	existeRed = func(*Net) bool { return false }
	asegurarHost = func() error { return nil }
	n := Plan(7, "abcd1234ef")
	if err := n.Setup(egress, nil, 1000); err != nil {
		t.Fatal(err)
	}
	return procs
}

// Hipótesis 2 del plan de rendimiento: `ip netns exec` (unshare de montajes y
// remontaje de /sys) se lanza varias veces por creación y no hace falta para
// entrar en un netns.
func TestSetupNoUsaIPNetnsExec(t *testing.T) {
	procs := grabarSetup(t, EgressInternet)
	t.Logf("%d procesos por Setup", len(procs))
	for _, p := range procs {
		if strings.HasPrefix(p, "ip netns exec") {
			t.Errorf("Setup lanza %.60q...: debe entrar en el netns con setns, sin `ip netns exec`", p)
		}
	}
}
