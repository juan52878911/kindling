package scheduler

import (
	"context"
	"testing"
	"time"
)

// B-05: evictLRU filtraba solo por inflight. Una instancia con una sesión
// reservada (pickInstance la entregó y el initialize va en camino, aún sin
// Begin) se congelaba en ese hueco y el initialize daba en un invitado
// pausado. Como en el segador, una reserva vigente la excluye.
func TestNoSeSacrificaUnaConSesionReservada(t *testing.T) {
	g, congeladas := gwConDosOciosas(t)
	delete(g.services, "nueva")
	g.services["vieja"].reservas = []time.Time{time.Now()}

	if got := g.evictLRU(context.Background(), "otro", ""); got != "" {
		t.Errorf("evictLRU = %q; 'vieja' tiene una sesión en camino y no debe tocarse", got)
	}
	if *congeladas != 0 {
		t.Errorf("se congelaron %d instancias; esperaba 0", *congeladas)
	}
}

// B-05: mientras se congela, la víctima debe estar marcada en adquiriendo,
// como la del segador: fuera del mapa y sin la marca, un acquire concurrente
// la veía "running" en List(), la adoptaba y el freeze llegaba por debajo.
func TestLaVictimaDelDesalojoQuedaMarcadaMientrasSeCongela(t *testing.T) {
	g, _ := gwConDosOciosas(t)
	var marcada bool
	g.freezeFn = func(id string) error {
		g.mu.Lock()
		marcada = g.adquiriendo[id]
		g.mu.Unlock()
		return nil
	}
	if got := g.evictLRU(context.Background(), "otro", ""); got != "vieja" {
		t.Fatalf("evictLRU = %q; quería 'vieja'", got)
	}
	if !marcada {
		t.Error("la víctima no estaba marcada en adquiriendo durante el freeze")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.adquiriendo["m-vieja"] {
		t.Error("la marca no se soltó tras el freeze")
	}
}
