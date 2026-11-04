package main

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"
)

// -session-idle 0 (o negativo) desactiva la cosecha en vez de entrar en pánico
// en time.NewTicker, que siendo PID 1 tumbaría al invitado.
func TestReapIdleDesactivadoSinPanico(t *testing.T) {
	for _, idle := range []time.Duration{0, -time.Second, 3} {
		b := &bridge{idle: idle, sessions: map[string]*session{}}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		done := make(chan struct{})
		go func() { defer close(done); b.reapIdle(ctx) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("idle %s: reapIdle no volvió", idle)
		}
		cancel()
	}
}

// El rastro del modo proxy no pasa del tope y, lleno, echa la más antigua.
func TestProxySeenAcotado(t *testing.T) {
	b := &bridge{}
	b.proxySeen = map[string]time.Time{"vieja": time.Now().Add(-time.Hour)}
	for i := 0; i < maxProxySeen+50; i++ {
		b.touchProxySession(fmt.Sprintf("s-%d", i))
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.proxySeen) != maxProxySeen {
		t.Fatalf("rastro con %d entradas; tope %d", len(b.proxySeen), maxProxySeen)
	}
	if _, ok := b.proxySeen["vieja"]; ok {
		t.Error("la sesión más antigua debía ser la primera en salir")
	}
	if _, ok := b.proxySeen[fmt.Sprintf("s-%d", maxProxySeen+49)]; !ok {
		t.Error("la última sesión tocada debía seguir rastreada")
	}
}

// Refrescar una sesión ya rastreada no desaloja a nadie aunque esté lleno.
func TestProxySeenRefrescoNoDesaloja(t *testing.T) {
	b := &bridge{proxySeen: map[string]time.Time{}}
	for i := 0; i < maxProxySeen; i++ {
		b.proxySeen[fmt.Sprintf("s-%d", i)] = time.Now()
	}
	b.touchProxySession("s-0")
	if len(b.proxySeen) != maxProxySeen {
		t.Fatalf("rastro con %d entradas tras refrescar", len(b.proxySeen))
	}
}

// matarYEsperar recoge al hijo: sin el Wait quedaría zombi fuera de la microVM.
func TestMatarYEsperarRecogeAlHijo(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	enSuPropioGrupo(cmd)
	if err := cmd.Start(); err != nil {
		t.Skipf("sin sleep: %v", err)
	}
	t0 := time.Now()
	matarYEsperar(cmd)
	if cmd.ProcessState == nil {
		t.Fatal("el hijo no se recogió")
	}
	if d := time.Since(t0); d > esperaHijoProxy {
		t.Errorf("tardó %s en recogerlo", d)
	}
	matarYEsperar(nil)
}
