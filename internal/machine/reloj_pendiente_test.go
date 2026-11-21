package machine

import (
	"context"
	"testing"
	"time"
)

// Con un resync pendiente que tarda 300 ms, ExecTarget espera a que acabe.
func TestExecTargetEsperaAlRelojPendiente(t *testing.T) {
	m := newTestManager(t)
	id := "eeee000000000010"
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.AllowExec = true
	m.mu.Unlock()

	fin := m.relojPendiente(id)
	go func() { time.Sleep(300 * time.Millisecond); fin() }()
	t0 := time.Now()
	_, _ = m.ExecTarget(context.Background(), id)
	if d := time.Since(t0); d < 250*time.Millisecond || d > 1500*time.Millisecond {
		t.Fatalf("ExecTarget tardó %s, quería ~300 ms (esperar al resync)", d)
	}
}

// Con uno que tarda 5 s, sigue a los 2 s (con aviso) sin esperar más.
func TestExecTargetSigueTrasElTopeConRelojPendiente(t *testing.T) {
	m := newTestManager(t)
	id := "eeee000000000011"
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.AllowExec = true
	m.mu.Unlock()

	fin := m.relojPendiente(id)
	defer fin()
	t0 := time.Now()
	_, _ = m.ExecTarget(context.Background(), id)
	if d := time.Since(t0); d < 1900*time.Millisecond || d > 3*time.Second {
		t.Fatalf("ExecTarget tardó %s, quería ~2 s (el tope)", d)
	}
}

// Sin resync pendiente no espera, y un ctx cancelado corta la espera.
func TestRelojPendienteSinPendienteNiCtx(t *testing.T) {
	m := newTestManager(t)
	t0 := time.Now()
	if m.esperarRelojPendiente(context.Background(), "nadie") {
		t.Fatal("sin pendiente no debe decir que sigue pendiente")
	}
	fin := m.relojPendiente("otra")
	defer fin()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if !m.esperarRelojPendiente(ctx, "otra") {
		t.Fatal("con el ctx vencido sigue pendiente")
	}
	if time.Since(t0) > time.Second {
		t.Fatal("tardó de más")
	}
}
