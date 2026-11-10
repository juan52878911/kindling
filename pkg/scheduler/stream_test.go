package scheduler

import (
	"context"
	"testing"
	"time"
)

// Un stream SSE abierto no es trabajo en vuelo: la instancia ociosa con un GET
// de sesión colgado se congela igual, y el stream se corta para que el cliente
// no se quede esperando a un invitado pausado. Antes el gateway contaba el GET
// con begin y la microVM no dormía nunca.
func TestStreamAbiertoNoImpideCongelar(t *testing.T) {
	g, _, congeladas := gwPausas(0)
	g.MachineTTL = -1
	e := &entry{machineID: "m-stream", ip: "10.0.0.2", lastUse: time.Now().Add(-time.Hour)}
	g.services["a"] = e

	ctx, cortar := context.WithCancel(context.Background())
	defer cortar()
	fin := g.BeginStream(e, cortar)
	defer fin()

	g.reapOnce(context.Background())
	if !congeladas["m-stream"] {
		t.Fatal("la instancia con solo un stream abierto debía congelarse")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("el stream de la instancia congelada no se cortó")
	}
	g.mu.Lock()
	n := len(e.streams)
	g.mu.Unlock()
	if n != 0 {
		t.Fatalf("quedan %d streams apuntados en la instancia congelada", n)
	}
}
