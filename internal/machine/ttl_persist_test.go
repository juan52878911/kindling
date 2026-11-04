package machine

import (
	"fmt"
	"testing"
	"time"
)

// BenchmarkTouchTTL mide lo que paga cada exec por reiniciar el reloj del TTL
// con 100 y 500 máquinas en el host: la foto de persist() copiaba TODAS las
// máquinas con el candado global tomado, en cada exec.
func BenchmarkTouchTTL(b *testing.B) {
	for _, n := range []int{100, 500} {
		m := benchManager(n)
		const id = "m-0000"
		m.byID[id].TTLSeconds = 600
		b.Run(fmt.Sprintf("maquinas=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				m.touchTTL(id)
			}
		})
	}
}

// Un exec reinicia el reloj del TTL en memoria al momento, pero no fuerza una
// foto del estado entero en cada llamada: solo cuando el reloj cruza a otra
// ventana de ttlPersistirCada.
func TestTouchTTLNoPersisteEnCadaExec(t *testing.T) {
	m := benchManager(3)
	const id = "m-0001"
	m.byID[id].TTLSeconds = 600
	fotos := func() uint64 {
		m.stateMu.Lock()
		defer m.stateMu.Unlock()
		return m.pendGen
	}

	m.touchTTL(id) // sin TTLAt previo: va a disco
	if fotos() != 1 {
		t.Fatalf("el primer touch tomó %d fotos, quería 1", fotos())
	}
	primero := *m.byID[id].TTLAt
	time.Sleep(time.Millisecond)
	for range 100 {
		m.touchTTL(id)
	}
	// Una más como mucho: los 100 pueden caer justo a caballo de dos ventanas.
	if got := fotos(); got > 2 {
		t.Errorf("100 exec seguidos tomaron %d fotos más del estado entero", got-1)
	}
	if !m.byID[id].TTLAt.After(primero) {
		t.Error("el reloj en memoria no se reinició")
	}

	// El reloj (y con él el de disco) se quedó atrás más de una ventana: el
	// siguiente exec sí escribe.
	antes := fotos()
	viejo := time.Now().Add(-ttlPersistirCada(600) - time.Second)
	m.byID[id].TTLAt = &viejo
	m.touchTTL(id)
	if got := fotos(); got != antes+1 {
		t.Errorf("con el reloj %s atrás, fotos = %d, quería %d", ttlPersistirCada(600), got, antes+1)
	}
}

// Lo que se pierde tras un reinicio está acotado: como mucho un 10 % del TTL
// y nunca más de 30 s, y al menos 1 s para que los TTL cortos coalescan algo.
func TestTTLPersistirCadaAcotado(t *testing.T) {
	for _, c := range []struct {
		ttl  int
		want time.Duration
	}{
		{5, time.Second},
		{60, 6 * time.Second},
		{600, 30 * time.Second},
		{86400, 30 * time.Second},
	} {
		if got := ttlPersistirCada(c.ttl); got != c.want {
			t.Errorf("ttlPersistirCada(%d) = %s, quería %s", c.ttl, got, c.want)
		}
	}
}
