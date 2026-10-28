package server

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/vz/internal/vnet"
)

// Un agente que contesta: la primera conexión se comprueba con /healthz y las
// siguientes ya no. Uno que acepta y calla (el vsock de una restauración que
// no entrega nada) da vnet.ErrCaminoMudo dentro del plazo.
func TestDialAgenteComprobado(t *testing.T) {
	var dials atomic.Int32
	contesta := func(ctx context.Context) (net.Conn, error) {
		dials.Add(1)
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			l, _ := bufio.NewReader(b).ReadString('\n')
			if l != "" {
				_, _ = b.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
			}
		}()
		return a, nil
	}
	d := dialAgenteComprobado(contesta)
	for i := 0; i < 3; i++ {
		c, err := d(context.Background())
		if err != nil {
			t.Fatalf("vuelta %d: %v", i, err)
		}
		c.Close()
	}
	if n := dials.Load(); n != 4 {
		t.Fatalf("%d dials, quiero 4 (la prueba solo en la primera)", n)
	}

	var abiertas []net.Conn
	calla := func(ctx context.Context) (net.Conn, error) {
		a, b := net.Pipe()
		abiertas = append(abiertas, b)
		go func() { _, _ = bufio.NewReader(b).ReadString('\n') }()
		return a, nil
	}
	defer func() {
		for _, c := range abiertas {
			c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	if _, err := dialAgenteComprobado(calla)(ctx); !errors.Is(err, vnet.ErrCaminoMudo) {
		t.Fatalf("err = %v, quiero ErrCaminoMudo", err)
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("tardó %s", d)
	}
}

// Justo tras restaurar, la primera conexión puede morir en silencio (el
// invitado aún no había atendido el TRANSPORT_RESET): con la segunda que
// contesta, el vsock vale y no se cae a TCP.
func TestDialAgenteComprobadoPrimeraMuda(t *testing.T) {
	var n atomic.Int32
	var mudas []net.Conn
	dial := func(ctx context.Context) (net.Conn, error) {
		a, b := net.Pipe()
		if n.Add(1) == 1 {
			mudas = append(mudas, b)
			go func() { _, _ = bufio.NewReader(b).ReadString('\n') }()
			return a, nil
		}
		go func() {
			defer b.Close()
			if l, _ := bufio.NewReader(b).ReadString('\n'); l != "" {
				_, _ = b.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
			}
		}()
		return a, nil
	}
	defer func() {
		for _, c := range mudas {
			c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := dialAgenteComprobado(dial)(ctx)
	if err != nil {
		t.Fatalf("con la segunda prueba buena: %v", err)
	}
	c.Close()
	if n.Load() != 3 {
		t.Fatalf("%d dials, quiero 3 (dos pruebas y la conexión)", n.Load())
	}
}

// El vsock que enmudece después de contestar: una conexión que escribió y no
// lee nada lanza la prueba; como no contesta, se cierra y los dials
// siguientes dan ErrCaminoMudo (el reenvío sigue por TCP).
func TestDialAgenteEnmudece(t *testing.T) {
	viejo := plazoMudez
	plazoMudez = 50 * time.Millisecond
	t.Cleanup(func() { plazoMudez = viejo })
	var calla atomic.Bool
	var colgadas []net.Conn
	var mu sync.Mutex
	dial := func(ctx context.Context) (net.Conn, error) {
		a, b := net.Pipe()
		if calla.Load() {
			mu.Lock()
			colgadas = append(colgadas, b)
			mu.Unlock()
			go func() { _, _ = io.Copy(io.Discard, b) }()
			return a, nil
		}
		go func() {
			defer b.Close()
			if l, _ := bufio.NewReader(b).ReadString('\n'); l != "" {
				_, _ = b.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
			}
		}()
		return a, nil
	}
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range colgadas {
			c.Close()
		}
	}()
	d := dialAgenteComprobado(dial)
	c, err := d(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()

	calla.Store(true)
	c, err = d(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("POST /resync HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var b [1]byte
	t0 := time.Now()
	if _, err := c.Read(b[:]); err == nil {
		t.Fatal("la conexión colgada contestó")
	}
	if time.Since(t0) > 2*time.Second {
		t.Fatalf("la conexión colgada no se cerró a tiempo (%s)", time.Since(t0))
	}
	for i := 0; ; i++ {
		_, err := d(context.Background())
		if errors.Is(err, vnet.ErrCaminoMudo) {
			break
		}
		if i > 100 {
			t.Fatalf("tras enmudecer el dial sigue dando %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// agenteDeQuita es un agente por net.Pipe que contesta a cada petición de una
// conexión (keep-alive) mientras calla sea false; después lee y no contesta.
type agenteDeQuita struct {
	calla atomic.Bool
	mu    sync.Mutex
	lados []net.Conn
}

func (a *agenteDeQuita) dial(ctx context.Context) (net.Conn, error) {
	c, s := net.Pipe()
	a.mu.Lock()
	a.lados = append(a.lados, s)
	a.mu.Unlock()
	go func() {
		r := bufio.NewReader(s)
		for {
			l, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if l == "\r\n" && !a.calla.Load() {
				if _, err := s.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")); err != nil {
					return
				}
			}
		}
	}()
	return c, nil
}

func (a *agenteDeQuita) cerrar() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.lados {
		c.Close()
	}
}

// Al confirmar la mudez se cierran TODAS las conexiones que esperaban, no
// solo la que lanzó la prueba; y una conexión que ya leyó (un keep-alive
// reutilizado) vuelve a vigilarse con su siguiente escritura.
func TestDialAgenteEnmudeceCierraTodas(t *testing.T) {
	viejo := plazoMudez
	plazoMudez = 50 * time.Millisecond
	t.Cleanup(func() { plazoMudez = viejo })
	ag := &agenteDeQuita{}
	defer ag.cerrar()
	d := dialAgenteComprobado(ag.dial)

	pedir := "POST /resync HTTP/1.1\r\n\r\n"
	vieja, err := d(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer vieja.Close()
	if _, err := vieja.Write([]byte(pedir)); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(vieja)
	for {
		l, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("primera respuesta: %v", err)
		}
		if l == "\r\n" {
			break
		}
	}

	ag.calla.Store(true)
	conns := []net.Conn{vieja}
	for i := 0; i < 3; i++ {
		c, err := d(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		conns = append(conns, c)
	}
	t0 := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, len(conns))
	for i, c := range conns {
		wg.Add(1)
		go func(i int, c net.Conn) {
			defer wg.Done()
			if _, err := c.Write([]byte(pedir)); err != nil {
				errs[i] = err
				return
			}
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			var b [1]byte
			var err error
			if i == 0 {
				_, err = r.Read(b[:])
			} else {
				_, err = c.Read(b[:])
			}
			if err == nil {
				errs[i] = errors.New("contestó")
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				errs[i] = err
			}
		}(i, c)
	}
	wg.Wait()
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("las conexiones colgadas tardaron %s en cerrarse", d)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("conexión %d: %v (quiero que se cierre)", i, err)
		}
	}
	if _, err := d(context.Background()); !errors.Is(err, vnet.ErrCaminoMudo) {
		t.Fatalf("tras enmudecer el dial da %v", err)
	}
}

// El plazo de quien llama no es mudez: un /healthz que tarda 300 ms (un
// invitado recién reanudado) contra un dial de 150 ms da DeadlineExceeded, y
// el dial siguiente sigue por vsock.
func TestDialAgentePlazoCortoNoEsMudez(t *testing.T) {
	var n atomic.Int32
	var mu sync.Mutex
	var lados []net.Conn
	dial := func(ctx context.Context) (net.Conn, error) {
		c, s := net.Pipe()
		mu.Lock()
		lados = append(lados, s)
		mu.Unlock()
		lenta := n.Add(1) == 1
		go func() {
			if l, _ := bufio.NewReader(s).ReadString('\n'); l == "" {
				return
			}
			if lenta {
				time.Sleep(300 * time.Millisecond)
			}
			_, _ = s.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
		}()
		return c, nil
	}
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range lados {
			c.Close()
		}
	}()
	d := dialAgenteComprobado(dial)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := d(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, quiero DeadlineExceeded", err)
	}
	c, err := d(context.Background())
	if err != nil {
		t.Fatalf("el dial siguiente: %v (quiero que siga por vsock)", err)
	}
	c.Close()
}
