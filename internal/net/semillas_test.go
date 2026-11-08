package net

import (
	"context"
	"slices"
	"testing"
	"time"
)

// El sembrado estático del allowlist va en el camino del arranque: antes se
// resolvía dominio a dominio sin plazo (hasta 3 × 2 s cada uno si el resolver
// perdía datagramas). Ahora todos a la vez y con plazoSemillas EN TOTAL: lo
// que llegue a tiempo se siembra, lo lento no frena el arranque.
func TestResolverSemillasEnParaleloYConPlazo(t *testing.T) {
	viejo := buscarIPv4
	t.Cleanup(func() { buscarIPv4 = viejo })
	buscarIPv4 = func(ctx context.Context, _ string, host string) []string {
		var espera time.Duration
		ip := map[string]string{"a.test": "93.184.216.34", "b.test": "93.184.216.35", "c.test": "93.184.216.34"}[host]
		switch host {
		case "lento.test":
			espera, ip = 6*time.Second, "1.2.3.4" // 3 intentos de 2 s
		default:
			espera = 100 * time.Millisecond
		}
		select {
		case <-time.After(espera):
			return []string{ip}
		case <-ctx.Done():
			return nil
		}
	}
	dominios := []string{"a.test", "lento.test", "b.test", "c.test", "a.test"}
	inicio := time.Now()
	got := resolverSemillas(dominios)
	if d := time.Since(inicio); d > plazoSemillas+200*time.Millisecond {
		t.Fatalf("resolverSemillas tardó %v; el plazo total es %v", d, plazoSemillas)
	}
	if quiere := []string{"93.184.216.34", "93.184.216.35"}; !slices.Equal(got, quiere) {
		t.Fatalf("semillas = %v, quería %v (sin repetir, sin el lento)", got, quiere)
	}
}
