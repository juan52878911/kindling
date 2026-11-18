package daemon

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// P8: cada exec, shell o fichero sondeaba el agente antes de hablarle, aunque
// el anterior acabara de hacerlo. Ahora se sondea una vez por vida del VMM; un
// fallo al hablarle (olvidarAgente) hace que el siguiente vuelva a sondear, y
// un sondeo fallido no se recuerda.
func TestAgenteSeSondeaUnaVezPorVida(t *testing.T) {
	s := &Server{}
	sondeos := 0
	var fallar error
	s.sondearAgente = func(context.Context, *api.Machine) error {
		sondeos++
		return fallar
	}
	desde := time.Now()
	mc := &api.Machine{ID: "aaaa000000000001", Name: "viva", PID: 10, StartedAt: &desde}
	pedir := func(m *api.Machine) error { return s.esperarAgenteUnaVez(context.Background(), m) }

	for range 6 {
		if err := pedir(mc); err != nil {
			t.Fatal(err)
		}
	}
	if sondeos != 1 {
		t.Fatalf("6 peticiones a la misma vida del VMM hicieron %d sondeos; quería 1", sondeos)
	}
	// Otro VMM (un thaw, un start): otro PID, otra vida.
	otra := *mc
	otra.PID = 11
	if err := pedir(&otra); err != nil || sondeos != 2 {
		t.Fatalf("con otro PID: %v, %d sondeos", err, sondeos)
	}
	s.olvidarAgente(mc.ID)
	if err := pedir(&otra); err != nil || sondeos != 3 {
		t.Fatalf("tras olvidarAgente: %v, %d sondeos", err, sondeos)
	}
	// Un sondeo que falla no se recuerda.
	s.olvidarAgente(mc.ID)
	fallar = errors.New("nadie escucha")
	if err := pedir(&otra); err == nil {
		t.Fatal("el fallo del sondeo no llegó")
	}
	fallar = nil
	if err := pedir(&otra); err != nil || sondeos != 5 {
		t.Fatalf("tras un sondeo fallido: %v, %d sondeos", err, sondeos)
	}
	// Sin PID o sin arranque no se sabe qué vida es: se sondea siempre.
	sinPID := &api.Machine{ID: "bbbb000000000001", StartedAt: &desde}
	_ = pedir(sinPID)
	_ = pedir(sinPID)
	if sondeos != 7 {
		t.Fatalf("sin PID: %d sondeos; quería 7", sondeos)
	}
}

func TestAgentesVistosAcotado(t *testing.T) {
	s := &Server{}
	viejo := maxAgentesVistos
	maxAgentesVistos = 2
	t.Cleanup(func() { maxAgentesVistos = viejo })
	desde := time.Now()
	for i := range 5 {
		s.marcarAgente(&api.Machine{ID: strconv.Itoa(i), PID: 1, StartedAt: &desde})
	}
	if n := len(s.agentesVistos); n > 2 {
		t.Fatalf("%d entradas, tope 2", n)
	}
}

// Lo que cuesta el sondeo que se ahorra, en el caso más barato (Linux: un
// connect y close a un puerto que escucha, aquí por loopback y no por el tap
// del invitado), frente a mirar la firma.
func BenchmarkSondeoAgente(b *testing.B) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	addr := ln.Addr().String()
	b.Run("connect", func(b *testing.B) {
		for range b.N {
			if err := waitPort(context.Background(), addr, time.Second); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("firma", func(b *testing.B) {
		s := &Server{}
		desde := time.Now()
		mc := &api.Machine{ID: "aaaa000000000001", PID: 10, StartedAt: &desde}
		s.marcarAgente(mc)
		for range b.N {
			if !s.agenteVisto(mc) {
				b.Fatal("no visto")
			}
		}
	})
}
