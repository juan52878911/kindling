package egress

import (
	"context"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Con el upstream colgado, un invitado que dispara consultas sin esperar no
// consigue más de MaxInFlight esperando a la vez: el resto es SERVFAIL en el
// sitio, sin abrir un socket más.
func TestEnVueloAcotado(t *testing.T) {
	p := NewPolicy()
	p.Set(Internet, nil)
	var ahora, maximo atomic.Int32
	suelta := make(chan struct{})
	r := &Resolver{Policy: p, Exchange: func(ctx context.Context, q []byte, _ bool) ([]byte, error) {
		n := ahora.Add(1)
		for {
			m := maximo.Load()
			if n <= m || maximo.CompareAndSwap(m, n) {
				break
			}
		}
		<-suelta
		ahora.Add(-1)
		return answer(q, 30, "93.184.215.14"), nil
	}}
	var wg sync.WaitGroup
	var servfail atomic.Int32
	for i := 0; i < 300; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp := r.Process(context.Background(), BuildQuery("example.com", 1), false); resp[3]&0x0F == 2 {
				servfail.Add(1)
			}
		}()
	}
	// Las que no caben vuelven enseguida; las otras esperan al upstream.
	deadline := time.Now().Add(3 * time.Second)
	for servfail.Load() < 300-MaxInFlight && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	close(suelta)
	wg.Wait()
	t.Logf("máximo en vuelo %d, SERVFAIL %d de 300", maximo.Load(), servfail.Load())
	if maximo.Load() > MaxInFlight {
		t.Fatalf("%d consultas esperaban a la vez al upstream (tope %d)", maximo.Load(), MaxInFlight)
	}
}

// Aunque el upstream conteste al momento, la tasa hacia él está acotada por
// el cubo de tokens.
func TestRitmoAcotado(t *testing.T) {
	p := NewPolicy()
	p.Set(Internet, nil)
	var enviadas atomic.Int32
	r := &Resolver{Policy: p, Exchange: func(ctx context.Context, q []byte, _ bool) ([]byte, error) {
		enviadas.Add(1)
		return answer(q, 30, "93.184.215.14"), nil
	}}
	inicio := time.Now()
	for i := 0; i < 5000; i++ {
		r.Process(context.Background(), BuildQuery("example.com", 1), false)
	}
	dur := time.Since(inicio).Seconds()
	limite := int32(Burst + Rate*dur + 10)
	t.Logf("%d de 5000 al upstream en %.2f s (límite %d)", enviadas.Load(), dur, limite)
	if enviadas.Load() > limite {
		t.Fatalf("%d consultas al upstream en %.2f s: sin ritmo", enviadas.Load(), dur)
	}
}

// Una respuesta con otro id o con otra pregunta no es la de esta consulta: ni
// se entrega ni siembra la allowlist.
func TestRespuestaAjenaNoSiembra(t *testing.T) {
	for nombre, estropea := range map[string]func([]byte) []byte{
		"otro id": func(b []byte) []byte { b[0] ^= 0xFF; return b },
		"otra pregunta": func(b []byte) []byte {
			return answer(BuildQuery("evil.example", 1), 30, "93.184.215.14")
		},
		"otro tipo": func(b []byte) []byte { b[len(BuildQuery("example.com", 1))-3] = 28; return b },
	} {
		t.Run(nombre, func(t *testing.T) {
			p := NewPolicy()
			p.Set(Allowlist, []string{"example.com"})
			r := &Resolver{Policy: p, Exchange: func(ctx context.Context, q []byte, _ bool) ([]byte, error) {
				return estropea(answer(q, 30, "93.184.215.14")), nil
			}}
			resp := r.Process(context.Background(), BuildQuery("example.com", 1), false)
			if resp[3]&0x0F != 2 {
				t.Errorf("rcode %d, want SERVFAIL", resp[3]&0x0F)
			}
			if p.AllowConn(netip.MustParseAddr("93.184.215.14")) {
				t.Error("una respuesta que no era de esta consulta sembró la allowlist")
			}
		})
	}
}
