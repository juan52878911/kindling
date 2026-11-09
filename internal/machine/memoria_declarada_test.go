package machine

import (
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// La admisión suma lo DECLARADO por las máquinas con VMM vivo (corriendo o en
// pausa, con su mem_max si lo tienen) y por los arranques en vuelo, y no deja
// pasar de KLING_MEM_OVERCOMMIT veces la RAM física; las congeladas y paradas
// no cuentan, y 0 lo apaga.
func TestAdmisionMemoriaDeclarada(t *testing.T) {
	antes := memoriaFisicaDeclarada
	memoriaFisicaDeclarada = func() int64 { return 100 } // 100 MiB de RAM: techo 400 con 4x
	t.Cleanup(func() { memoriaFisicaDeclarada = antes })
	t.Setenv("KLING_MEM_OVERCOMMIT", "")

	m := newTestManager(t)
	a, b, c, d := m.addForTest("a"), m.addForTest("b"), m.addForTest("c"), m.addForTest("d")
	m.mu.Lock()
	a.State, a.MemMiB = api.StateRunning, 100
	b.State, b.MemMiB, b.MemMaxMiB = api.StatePaused, 50, 150 // cuenta su techo
	c.State, c.MemMiB = api.StateWarm, 1000                   // congelada: sin proceso
	d.State, d.MemMiB = api.StateStopped, 1000
	m.mu.Unlock()

	// Vivas: 100 + 150 = 250; caben 150 más.
	release, err := m.reserveMemory(100, "")
	if err != nil {
		t.Fatalf("250+100 de 400 debía caber: %v", err)
	}
	// Con esa en vuelo (350), 60 más no caben.
	if _, err := m.reserveMemory(60, ""); !api.IsInsufficientMemory(err) {
		t.Fatalf("350+60 de 400: quería memoria insuficiente, got %v", err)
	}
	release()
	r2, err := m.reserveMemory(60, "")
	if err != nil {
		t.Fatalf("liberada la reserva, 250+60 debía caber: %v", err)
	}
	r2()

	t.Setenv("KLING_MEM_OVERCOMMIT", "0")
	m.mu.Lock()
	err = m.checkMemoriaDeclaradaLocked(10000)
	m.mu.Unlock()
	if err != nil {
		t.Fatalf("KLING_MEM_OVERCOMMIT=0 apaga el techo: %v", err)
	}
	t.Setenv("KLING_MEM_OVERCOMMIT", "1.5") // techo 150, ya hay 250 vivas
	m.mu.Lock()
	err = m.checkMemoriaDeclaradaLocked(1)
	m.mu.Unlock()
	if !api.IsInsufficientMemory(err) {
		t.Fatalf("con 1.5x: quería memoria insuficiente, got %v", err)
	}
}
