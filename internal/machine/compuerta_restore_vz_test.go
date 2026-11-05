//go:build darwin

package machine

import (
	"context"
	"errors"
	"sync/atomic"
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

// Con memoria de sobra entran lanzamientoPlataforma restauraciones a la vez y
// la siguiente espera; un arranque en frío no espera detrás de ellas.
func TestCompuertaRestoreVZ(t *testing.T) {
	conNivelMemoriaVZ(t, 80)
	m := &Manager{launchGate: make(chan struct{}, lanzamientoPlataforma())}
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
	frio, err := m.enterBoot(context.Background())
	if err != nil {
		t.Fatalf("un arranque en frío no espera a las restauraciones: %v", err)
	}
	frio()
	frio() // soltar dos veces no suelta un hueco ajeno
	for _, r := range sueltas {
		r()
		r()
	}
	if len(m.launchGate) != 0 || len(compuertaFrioVZ) != 0 {
		t.Fatalf("quedan huecos tomados: %d/%d", len(m.launchGate), len(compuertaFrioVZ))
	}
}

// Con el nivel de memoria en el freno se restaura de una en una.
func TestCompuertaRestoreVZNivelBajo(t *testing.T) {
	conNivelMemoriaVZ(t, nivelRestoreVZ)
	m := &Manager{launchGate: make(chan struct{}, lanzamientoPlataforma())}
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
	if len(m.launchGate) != 0 || len(compuertaRestoreBajaVZ) != 0 {
		t.Fatalf("quedan huecos tomados: %d/%d", len(m.launchGate), len(compuertaRestoreBajaVZ))
	}
}

// Con kling-hv el nivel bajo no frena: ni la espera de esperaNivelRestoreVZ
// ni el "de una en una" (restaurar no copia el volcado entero). La compuerta
// general sí sigue: entran restoresSimultaneosHV y la siguiente espera.
func TestCompuertaRestoreHVNivelBajoNoFrena(t *testing.T) {
	if err := ElegirBackend(BackendHV); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ElegirBackend(BackendVZ) })
	conNivelMemoriaVZ(t, nivelRestoreVZ)
	esperaNivelRestoreVZ = time.Second // si esperase, se notaría
	var consultas atomic.Int32
	nivelMemoriaVZ = func() int { consultas.Add(1); return nivelRestoreVZ }
	m := &Manager{launchGate: make(chan struct{}, lanzamientoPlataforma())}
	t0 := time.Now()
	var sueltas []func()
	for i := 0; i < restoresSimultaneosHV; i++ {
		r, err := m.enterRestore(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		sueltas = append(sueltas, r)
	}
	if d := time.Since(t0); d > 500*time.Millisecond || consultas.Load() != 0 {
		t.Fatalf("hv esperó al nivel: %v, %d consultas", d, consultas.Load())
	}
	if len(compuertaRestoreBajaVZ) != 0 {
		t.Fatal("hv tomó el hueco de \"de una en una\"")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := m.enterRestore(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("la restauración %d debía esperar a la compuerta: %v", restoresSimultaneosHV+1, err)
	}
	for _, r := range sueltas {
		r()
	}
	if len(m.launchGate) != 0 {
		t.Fatalf("quedan huecos tomados: %d", len(m.launchGate))
	}
	// Y las copias de un fork entran juntas por esa misma compuerta.
	if n := copiasForkALaVez(); n != restoresSimultaneosHV {
		t.Fatalf("copias de fork a la vez con hv = %d, quiero %d", n, restoresSimultaneosHV)
	}
	if err := ElegirBackend(BackendVZ); err != nil {
		t.Fatal(err)
	}
	if n := copiasForkALaVez(); n != 1 {
		t.Fatalf("copias de fork a la vez con vz = %d, quiero 1", n)
	}
}
