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
	oldO, oldE, oldB, oldR, oldH, oldX, oldI, oldA, oldC, oldP := ordenEnNetns, ejecutar, ipBatch, ejecutarRestore, hayRestore, existeRed, hayIp6tables, asegurarHost, hayConntrack, existeRuta
	t.Cleanup(func() {
		ordenEnNetns, ejecutar, ipBatch, ejecutarRestore, hayRestore, existeRed, hayIp6tables, asegurarHost, hayConntrack, existeRuta = oldO, oldE, oldB, oldR, oldH, oldX, oldI, oldA, oldC, oldP
	})
	// Con conntrack instalado y sin red previa: una alta nueva no lo lanza.
	hayConntrack = func() bool { return true }
	existeRuta = func(string) bool { return false }
	var procs []string
	ordenEnNetns = func(ns string, args ...string) error {
		procs = append(procs, "en-netns "+ns+" "+strings.Join(args, " "))
		return nil
	}
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
	vaciarConntrackHost()
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

// R7: una alta nueva no lanza conntrack (no hay nada que borrar).
func TestSetupDeMaquinaNuevaNoLanzaConntrack(t *testing.T) {
	for _, p := range grabarSetup(t, EgressInternet) {
		if strings.HasPrefix(p, "conntrack ") {
			t.Errorf("Setup lanzó %q", p)
		}
	}
}

// Con el fondo de ranuras apagado, la secuencia de procesos de Setup es la de
// siempre: netns add, ip batch del host, ip batch del netns, sysctl, ip6tables
// y un único iptables-restore con el DNAT y las reglas de salida.
func TestSetupSecuenciaDeOro(t *testing.T) {
	procs := grabarSetup(t, EgressInternet)
	var got []string
	for _, p := range procs {
		if f := strings.Fields(p); len(f) > 4 {
			p = strings.Join(f[:4], " ")
		}
		got = append(got, p)
	}
	want := []string{
		"ip netns add kl-abcd1234",
		"ip-batch ",
		"ip-batch kl-abcd1234",
		"en-netns kl-abcd1234 sysctl -q",
		"en-netns kl-abcd1234 ip6tables -A",
		"iptables-restore kl-abcd1234",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("secuencia de Setup:\n got %q\nwant %q", got, want)
	}
}
