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
	fin := g.BeginStream(e, "", cortar)
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

// Una sesión tiene un stream SSE como mucho: N GET seguidos dejan uno abierto,
// el último, y los anteriores se cortan en vez de acumularse.
func TestUnStreamPorSesion(t *testing.T) {
	g, _, _ := gwPausas(0)
	e := &entry{machineID: "m-s", ip: "10.0.0.2", lastUse: time.Now()}
	g.services["a"] = e

	var ctxs []context.Context
	for i := 0; i < 5; i++ {
		ctx, cortar := context.WithCancel(context.Background())
		defer cortar()
		defer g.BeginStream(e, "S1", cortar)()
		ctxs = append(ctxs, ctx)
	}
	g.mu.Lock()
	n := len(e.streams)
	g.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d streams abiertos para una sesión; quería 1", n)
	}
	for i, ctx := range ctxs[:len(ctxs)-1] {
		if ctx.Err() == nil {
			t.Errorf("el stream %d no se cortó al abrir otro de la misma sesión", i)
		}
	}
	if ctxs[len(ctxs)-1].Err() != nil {
		t.Error("el último stream de la sesión se cortó")
	}
}

// Una instancia que ya salió del mapa no admite streams: se cortan al momento
// en vez de quedar colgados de una entrada que nadie va a cortar.
func TestStreamSobreInstanciaRetirada(t *testing.T) {
	g, _, _ := gwPausas(0)
	e := &entry{machineID: "m-fuera", ip: "10.0.0.2", lastUse: time.Now()}
	ctx, cortar := context.WithCancel(context.Background())
	defer cortar()
	g.BeginStream(e, "S1", cortar)()
	if ctx.Err() == nil {
		t.Fatal("el stream sobre una instancia fuera del mapa no se cortó")
	}
	if len(e.streams) != 0 {
		t.Fatalf("quedan %d streams apuntados en una instancia fuera del mapa", len(e.streams))
	}
}

// PeekRoute no cuenta como uso: ni la ruta ni la sesión aislada ni su
// instancia refrescan lastUse.
func TestPeekRouteNoRefresca(t *testing.T) {
	g, _, _ := gwPausas(0)
	antes := time.Now().Add(-time.Hour)
	e := &entry{machineID: "m-p", ip: "10.0.0.2", lastUse: antes}
	g.services["a"] = e
	g.routes = map[string]*sessionRoute{}
	g.routes["S1"] = &sessionRoute{service: "a", machineID: "m-p", lastUse: antes}
	g.aisladas = map[string]*aislada{}
	g.aisladas["S1"] = &aislada{key: "S1", service: "a", machineID: "m-p", lastUse: antes}
	if g.PeekRoute("S1") == nil {
		t.Fatal("PeekRoute no encontró la sesión")
	}
	if !e.lastUse.Equal(antes) || !g.routes["S1"].lastUse.Equal(antes) || !g.aisladas["S1"].lastUse.Equal(antes) {
		t.Fatal("PeekRoute refrescó lastUse")
	}
}
