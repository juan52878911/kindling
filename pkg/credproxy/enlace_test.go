package credproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ecoTCP levanta un servidor que devuelve lo que recibe y dice cuántas
// conexiones aceptó.
func ecoTCP(t *testing.T) (addr string, aceptadas *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("sin TCP local: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	aceptadas = &atomic.Int32{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			aceptadas.Add(1)
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String(), aceptadas
}

// enlaceDePrueba arranca un Enlace con el destino relajado (el eco vive en el
// loopback, que en producción no es un destino válido) y devuelve dónde
// escucha y dónde audita.
func enlaceDePrueba(t *testing.T, o LinkOptions) (*Enlace, string, string) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("sin TCP local: %v", err)
	}
	audPath := filepath.Join(t.TempDir(), AuditFile)
	aud := NewAuditor(audPath, nil)
	if o.Destino == nil {
		o.Destino = func(ap netip.AddrPort) error {
			if !ap.Addr().IsLoopback() {
				return errors.New("solo loopback en el test")
			}
			return nil
		}
	}
	o.Auditor = aud
	if o.Name == "" {
		o.Name = "api.graph:8080"
	}
	e := NuevoEnlace(o)
	go func() { _ = e.Serve(ln) }()
	t.Cleanup(func() { _ = e.Close(); _ = aud.Close() })
	return e, ln.Addr().String(), audPath
}

func registrosEnlace(t *testing.T, e *Enlace, path string) []Record {
	t.Helper()
	// Las sesiones que el cliente ya cerró terminan solas: se espera a que lo
	// hagan, o Close las contaría como cortadas.
	for i := 0; i < 200; i++ {
		e.mu.Lock()
		n := len(e.sesiones)
		e.mu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = e.Close()
	_ = e.o.Auditor.Close()
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

func ecoEnlace(t *testing.T, addr, msg string) (string, error) {
	t.Helper()
	c, err := net.DialTimeout("tcp4", addr, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		return "", err
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// Resolve se pide en cada conexión: lo que cambia entre dos cuenta.
func TestEnlaceResuelveEnCadaConexion(t *testing.T) {
	destino, _ := ecoTCP(t)
	var llamadas atomic.Int32
	var fallar atomic.Bool
	e, addr, audPath := enlaceDePrueba(t, LinkOptions{Resolve: func(ctx context.Context) (string, string, error) {
		llamadas.Add(1)
		if fallar.Load() {
			return "", "", errors.New("node db is frozen")
		}
		return destino, "b000000000000001", nil
	}})

	for i := 0; i < 3; i++ {
		got, err := ecoEnlace(t, addr, "hola")
		if err != nil || got != "hola" {
			t.Fatalf("conexión %d: %q %v", i, got, err)
		}
	}
	if n := llamadas.Load(); n != 3 {
		t.Fatalf("Resolve se llamó %d veces para 3 conexiones", n)
	}
	fallar.Store(true)
	if got, err := ecoEnlace(t, addr, "hola"); err == nil {
		t.Fatalf("con Resolve fallando la conexión pasó: %q", got)
	}
	rs := registrosEnlace(t, e, audPath)
	var ok, denegadas int
	for _, r := range rs {
		if r.Kind != KindLink || r.Host != "api.graph:8080" {
			t.Errorf("registro inesperado: %+v", r)
		}
		if r.Reason == "" && r.Upstream == "machine:b000000000000001" && r.ReqBytes == 4 && r.RespBytes == 4 {
			ok++
		}
		if r.Reason == ReasonMachineUnavailable && r.Denied {
			denegadas++
		}
	}
	if ok != 3 || denegadas != 1 {
		t.Fatalf("auditoría: %d buenas y %d denegadas, esperaba 3 y 1: %+v", ok, denegadas, rs)
	}
}

// Lo resuelto tiene que ser un destino de kindling: una dirección de la LAN
// no se marca aunque Resolve la devuelva.
func TestEnlaceDestinoFueraDeKindling(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	var marcado atomic.Bool
	e := NuevoEnlace(LinkOptions{Name: "x.graph:1",
		Resolve: func(context.Context) (string, string, error) { return "192.168.1.10:5432", "b1", nil },
		Dial: func(context.Context, string, string) (net.Conn, error) {
			marcado.Store(true)
			return nil, errors.New("no")
		}})
	go func() { _ = e.Serve(ln) }()
	defer e.Close()
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("la conexión siguió abierta")
	}
	c.Close()
	if marcado.Load() {
		t.Fatal("se marcó una dirección que no es de kindling")
	}
	if err := destinoMaquinaValido(netip.MustParseAddrPort("172.30.0.6:8080")); err != nil {
		t.Errorf("la IP de un netns debería valer: %v", err)
	}
	if err := destinoMaquinaValido(netip.MustParseAddrPort("192.168.1.10:5432")); err == nil {
		t.Error("una IP de la LAN no es un destino de kindling")
	}
}

// Invalidar corta las sesiones hacia esa máquina y deja las demás.
func TestEnlaceInvalidar(t *testing.T) {
	destino, _ := ecoTCP(t)
	maq := atomic.Value{}
	maq.Store("b000000000000001")
	e, addr, audPath := enlaceDePrueba(t, LinkOptions{Resolve: func(context.Context) (string, string, error) {
		return destino, maq.Load().(string), nil
	}})
	abrir := func() net.Conn {
		c, err := net.Dial("tcp4", addr)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(c, make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
		return c
	}
	c1 := abrir()
	defer c1.Close()
	maq.Store("b000000000000002")
	c2 := abrir()
	defer c2.Close()

	if n := e.Invalidar("b000000000000009"); n != 0 {
		t.Fatalf("Invalidar de otra máquina cortó %d", n)
	}
	if n := e.Invalidar("b000000000000001"); n != 1 {
		t.Fatalf("Invalidar cortó %d sesiones, quería 1", n)
	}
	if _, err := c1.Read(make([]byte, 1)); err == nil {
		t.Fatal("la sesión hacia la máquina invalidada sigue viva")
	}
	if _, err := c2.Write([]byte("y")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(c2, make([]byte, 1)); err != nil {
		t.Fatalf("la otra sesión se cortó: %v", err)
	}
	c1.Close()
	c2.Close()
	rs := registrosEnlace(t, e, audPath)
	inv := 0
	for _, r := range rs {
		if r.Reason == ReasonInvalidated {
			inv++
		}
	}
	if inv != 1 {
		t.Fatalf("auditoría: %d invalidadas, quería 1: %+v", inv, rs)
	}
}

// Una sesión que aún resuelve (despertando al destino) se corta al invalidar
// su nodo destino, y no al invalidar otra máquina cualquiera.
func TestEnlaceInvalidarPendiente(t *testing.T) {
	entrar := make(chan struct{})
	salir := make(chan error, 1)
	e, addr, _ := enlaceDePrueba(t, LinkOptions{Target: "nodo-db", Resolve: func(ctx context.Context) (string, string, error) {
		close(entrar)
		<-ctx.Done()
		salir <- ctx.Err()
		return "", "", ctx.Err()
	}})
	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case <-entrar:
	case <-time.After(3 * time.Second):
		t.Fatal("Resolve no llegó a llamarse")
	}
	if n := e.Invalidar("b000000000000042"); n != 0 {
		t.Fatalf("Invalidar de otra máquina cortó %d pendientes", n)
	}
	if n := e.Invalidar("nodo-db"); n != 1 {
		t.Fatalf("Invalidar del nodo destino cortó %d pendientes, quería 1", n)
	}
	select {
	case err := <-salir:
		if err == nil {
			t.Fatal("la resolución no se canceló")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("la resolución siguió esperando tras cortar la sesión")
	}
}

// MaxConns: las que esperan también cuentan; la siguiente se cierra en el
// acto y queda como busy.
func TestEnlaceTopeDeConexiones(t *testing.T) {
	bloqueo := make(chan struct{})
	var dentro sync.WaitGroup
	dentro.Add(2)
	e, addr, audPath := enlaceDePrueba(t, LinkOptions{MaxConns: 2, Resolve: func(ctx context.Context) (string, string, error) {
		dentro.Done()
		select {
		case <-bloqueo:
		case <-ctx.Done():
		}
		return "", "", errors.New("fin")
	}})
	var cs []net.Conn
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp4", addr)
		if err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	dentro.Wait()
	c3, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = c3.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c3.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("la tercera conexión no se cerró en el acto: %v", err)
	}
	c3.Close()
	close(bloqueo)
	for _, c := range cs {
		c.Close()
	}
	rs := registrosEnlace(t, e, audPath)
	busy := 0
	for _, r := range rs {
		if r.Reason == ReasonBusy {
			busy++
		}
	}
	if busy != 1 {
		t.Fatalf("auditoría: %d busy, quería 1: %+v", busy, rs)
	}
}

// Los motivos de un Resolve fallido llegan a la auditoría.
func TestEnlaceMotivos(t *testing.T) {
	casos := map[error]string{
		ErrEnlaceOcupado:                    ReasonBusy,
		ErrSinCapacidad:                     ReasonNoCapacity,
		errors.New("node db doesn't exist"): ReasonMachineUnavailable,
	}
	for err, want := range casos {
		if got := motivoResolucion(err); got != want {
			t.Errorf("%v -> %q, quería %q", err, got, want)
		}
	}
}
