//go:build darwin

package machine

import (
	"context"
	"errors"
	"testing"
	"time"
)

func conNivelMemoriaVZ(t *testing.T, nivel int) {
	t.Helper()
	viejo, espera := nivelMemoriaVZ, esperaNivelRestoreVZ
	nivelMemoriaVZ = func() int { return nivel }
	esperaNivelRestoreVZ = 20 * time.Millisecond
	t.Cleanup(func() { nivelMemoriaVZ, esperaNivelRestoreVZ = viejo, espera })
}

// Con memoria de sobra entran restoresSimultaneosVZ restauraciones a la vez y
// la siguiente espera; la puerta de arranques en frío ni se toca.
func TestCompuertaRestoreVZ(t *testing.T) {
	conNivelMemoriaVZ(t, 80)
	m := &Manager{launchGate: make(chan struct{}, 1)}
	var sueltas []func()
	for i := 0; i < restoresSimultaneosVZ; i++ {
		r, err := m.enterRestore(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		sueltas = append(sueltas, r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := m.enterRestore(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("la restauración %d debía esperar: %v", restoresSimultaneosVZ+1, err)
	}
	frio, err := m.enterLaunch(context.Background())
	if err != nil {
		t.Fatalf("un arranque en frío no espera a las restauraciones: %v", err)
	}
	frio()
	for _, r := range sueltas {
		r()
		r() // soltar dos veces no suelta un hueco ajeno
	}
	if len(compuertaRestoreVZ) != 0 {
		t.Fatalf("quedan %d huecos tomados", len(compuertaRestoreVZ))
	}
}

// Con el nivel de memoria en el freno se restaura de una en una.
func TestCompuertaRestoreVZNivelBajo(t *testing.T) {
	conNivelMemoriaVZ(t, nivelRestoreVZ)
	m := &Manager{launchGate: make(chan struct{}, 1)}
	r, err := m.enterRestore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, err := m.enterRestore(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("con el nivel bajo la segunda debía esperar: %v", err)
	}
	r()
	if len(compuertaRestoreVZ) != 0 || len(compuertaRestoreBajaVZ) != 0 {
		t.Fatalf("quedan huecos tomados: %d/%d", len(compuertaRestoreVZ), len(compuertaRestoreBajaVZ))
	}
}
