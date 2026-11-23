package main

import (
	"strings"
	"testing"
	"time"
)

// Con tope de varias sesiones, llegar al tope no puede matar una conversacion
// viva: una sesion sin llamadas durante un rato es normal (el modelo pensando),
// y el gateway solo escala a una replica cuando el puente contesta "session
// limit". Reciclar la ociosa mas antigua le quita la ocasion y deja a ese
// cliente contra un proceso nuevo y vacio.
func TestConTopeMayorQueUnoNoSeRecicla(t *testing.T) {
	b := testBridge("cat")
	b.maxSessions = 2
	defer b.closeAll()

	var viejas []*session
	for i := 0; i < 2; i++ {
		s, _, err := b.resolve("", true)
		if err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
		b.release(s)
		viejas = append(viejas, s)
	}
	b.mu.Lock()
	for _, s := range viejas {
		s.lastUse = time.Now().Add(-time.Hour)
	}
	b.mu.Unlock()

	_, _, err := b.resolve("", true)
	if err == nil || !strings.Contains(err.Error(), "session limit") {
		t.Fatalf("al tope con 2 sesiones, se esperaba \"session limit\" y no reciclar: err=%v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range viejas {
		if _, sigue := b.sessions[s.id]; !sigue {
			t.Errorf("la sesion %s se recicló con tope 2", s.id)
		}
	}
}

// Con tope 1 se conserva el reciclado: sin el, un cliente que se reconecta
// (su sesion anterior aun contada) quedaria fuera hasta que la cosechara el
// reaper de inactividad, y con las replicas agotadas no habria a donde ir.
func TestConTopeUnoSeReciclaLaOciosa(t *testing.T) {
	b := testBridge("cat")
	b.maxSessions = 1
	defer b.closeAll()

	vieja, _, err := b.resolve("", true)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	b.release(vieja)
	b.mu.Lock()
	vieja.lastUse = time.Now().Add(-time.Hour)
	b.mu.Unlock()

	nueva, _, err := b.resolve("", true)
	if err != nil {
		t.Fatalf("con tope 1 y la anterior ociosa, el initialize debe entrar: %v", err)
	}
	b.release(nueva)
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, sigue := b.sessions[vieja.id]; sigue {
		t.Error("con tope 1, la sesion ociosa no se recicló")
	}
}
