package main

import (
	"context"
	"testing"
	"time"
)

// Las capturas del muro no se acumulan: un nombre que no es un teléfono no deja
// entrada, y la de un teléfono que deja de correr se suelta al refrescar.
func TestWallShotsAcotadas(t *testing.T) {
	a, f, _ := testApp(t)
	f.addGolden("phone-golden")
	ctx := context.Background()
	if err := a.up(ctx, 2, "phone-golden"); err != nil {
		t.Fatal(err)
	}
	w := newWall(a, 2)
	for _, n := range []string{"no-existe-1", "no-existe-2"} {
		if _, err := w.shot(ctx, n); err == nil {
			t.Fatalf("captura de %s sin error", n)
		}
	}
	if _, err := w.shot(ctx, "phone-2"); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	if len(w.shots) != 1 || w.shots["phone-2"] == nil {
		t.Fatalf("capturas en caché: %v", w.shots)
	}
	w.mu.Unlock()

	m, _ := a.phone(ctx, "2")
	_, _ = f.Pause(ctx, m.ID)
	w.mu.Lock()
	w.listAt = time.Time{}
	w.mu.Unlock()
	if _, err := w.running(ctx); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.shots) != 0 {
		t.Fatalf("la captura del teléfono pausado sigue en caché: %v", w.shots)
	}
}
