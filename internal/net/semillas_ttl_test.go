package net

import "testing"

// Las IPs del sembrado estático caducan como las que aprende el resolver: una
// IP que el dominio deja de usar no se queda abierta toda la vida del netns.
func TestSemillasCaducan(t *testing.T) {
	viejo := anadirSemilla
	t.Cleanup(func() { anadirSemilla = viejo })
	type alta struct {
		ns, set, ip string
		timeout     int
	}
	var altas []alta
	anadirSemilla = func(ns, set, ip string, timeout int) error {
		altas = append(altas, alta{ns, set, ip, timeout})
		return nil
	}
	sembrarSemillas("kns-1", "kl-1", []string{"93.184.216.34", "93.184.216.35"})
	if len(altas) != 2 {
		t.Fatalf("se sembraron %d IPs; quería 2", len(altas))
	}
	for _, a := range altas {
		if a.ns != "kns-1" || a.set != "kl-1" {
			t.Errorf("semilla %s en %s/%s; quería kns-1/kl-1", a.ip, a.ns, a.set)
		}
		if a.timeout <= 0 || a.timeout > dnsMaxTTL {
			t.Errorf("semilla %s con timeout %d: 0 es permanente; quería (0, %d]", a.ip, a.timeout, dnsMaxTTL)
		}
	}
}
