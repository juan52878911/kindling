package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

// invitadoEco hace de puerto del invitado: devuelve lo que recibe.
func invitadoEco(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

// servidorTunel monta un daemon con la máquina "web" (congelada, con el puerto
// 5000 en kling.ports y reenviado a fwd) escuchando en un socket unix.
// Devuelve el cliente y cuántas veces se la despertó.
func servidorTunel(t *testing.T, fwd string, topes topesTunel) (*api.Client, *Server, *atomic.Int32) {
	t.Helper()
	root := t.TempDir()
	web := &api.Machine{ID: "t1t1t1t1t1t1t1t1", Name: "web", State: api.StateWarm,
		Labels: map[string]string{api.LabelPorts: "5000"}}
	b, _ := json.Marshal([]*api.Machine{web})
	if err := os.WriteFile(filepath.Join(root, "state.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := machine.NewManager(root, "", "", events.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	var despertada atomic.Int32
	s := &Server{mgr: mgr, root: root, topesTunel: topes}
	s.thaw = func(ctx context.Context, id string) (*api.Machine, error) {
		despertada.Add(1)
		m := *web
		m.State = api.StateRunning
		m.Forwards = map[string]string{"5000": fwd, "5001": fwd, "8080": fwd}
		return &m, nil
	}
	sock := filepath.Join(t.TempDir(), "k.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s.routes()}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return api.NewClient("unix://" + sock), s, &despertada
}

func TestTunelTCPEcoBinario(t *testing.T) {
	cli, _, despertada := servidorTunel(t, invitadoEco(t), topesTunel{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := cli.DialTCP(ctx, "web", 5000)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if despertada.Load() != 1 {
		t.Fatalf("despertó %d veces, quería 1 (estaba congelada)", despertada.Load())
	}
	// Binario: todos los valores de byte, varias veces, y más de un búfer.
	envio := make([]byte, 300_000)
	for i := range envio {
		envio[i] = byte(i * 7)
	}
	go func() { _, _ = c.Write(envio) }()
	vuelta := make([]byte, len(envio))
	if _, err := io.ReadFull(c, vuelta); err != nil {
		t.Fatal(err)
	}
	for i := range envio {
		if envio[i] != vuelta[i] {
			t.Fatalf("byte %d: %x != %x", i, vuelta[i], envio[i])
		}
	}
}

func TestTunelTCPErrores(t *testing.T) {
	// Un puerto cerrado: se reserva y se suelta.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	cerrado := l.Addr().String()
	l.Close()
	cli, _, _ := servidorTunel(t, cerrado, topesTunel{plazo: 300 * time.Millisecond})
	ctx := context.Background()

	_, err := cli.DialTCP(ctx, "web", 5000)
	var se *api.StatusError
	if !errors.As(err, &se) || se.Code != http.StatusBadGateway || !strings.Contains(err.Error(), "nothing answers on port 5000") {
		t.Fatalf("puerto cerrado: %v", err)
	}
	// No publicado (no está en kling.ports) y el del agente: 403.
	for _, p := range []int{6000, api.GuestPort} {
		_, err = cli.DialTCP(ctx, "web", p)
		if !errors.As(err, &se) || se.Code != http.StatusForbidden || !strings.Contains(err.Error(), "not published") {
			t.Fatalf("puerto %d: %v", p, err)
		}
	}
	if _, err = cli.DialTCP(ctx, "web", 0); !errors.As(err, &se) || se.Code != http.StatusBadRequest {
		t.Fatalf("puerto 0: %v", err)
	}
	if _, err = cli.DialTCP(ctx, "nada", 5000); !errors.As(err, &se) || se.Code != http.StatusNotFound {
		t.Fatalf("máquina que no existe: %v", err)
	}
}

func TestTunelTCPTopeDeConexiones(t *testing.T) {
	cli, s, _ := servidorTunel(t, invitadoEco(t), topesTunel{max: 2})
	ctx := context.Background()
	a, err := cli.DialTCP(ctx, "web", 5000)
	if err != nil {
		t.Fatal(err)
	}
	b, err := cli.DialTCP(ctx, "web", 5000)
	if err != nil {
		t.Fatal(err)
	}
	_, err = cli.DialTCP(ctx, "web", 5000)
	var se *api.StatusError
	if !errors.As(err, &se) || se.Code != http.StatusTooManyRequests {
		t.Fatalf("tercera: %v", err)
	}
	a.Close()
	// La plaza se suelta cuando el daemon ve el cierre.
	for i := 0; ; i++ {
		c, err := cli.DialTCP(ctx, "web", 5000)
		if err == nil {
			c.Close()
			break
		}
		if i > 100 {
			t.Fatalf("la plaza no se soltó: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	b.Close()
	for i := 0; i < 100; i++ {
		s.tuneles.mu.Lock()
		n := len(s.tuneles.n)
		s.tuneles.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("quedan túneles contados tras cerrarlos todos")
}

func TestTunelTCPInactividad(t *testing.T) {
	cli, _, _ := servidorTunel(t, invitadoEco(t), topesTunel{inactividad: 200 * time.Millisecond})
	c, err := cli.DialTCP(context.Background(), "web", 5000)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Sin datos, el daemon cuelga: la lectura acaba sola.
	hecho := make(chan error, 1)
	go func() { _, err := c.Read(make([]byte, 1)); hecho <- err }()
	select {
	case err := <-hecho:
		if err == nil {
			t.Fatal("leyó datos de la nada")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("el túnel inactivo no se cerró")
	}
}

// Sin Upgrade, 426; con política de authz y sin rol, 403.
func TestTunelTCPSinUpgradeYSinPermiso(t *testing.T) {
	s := servidorAuthz(t, politicaDePrueba(t))
	h := s.routes()
	if rr := como(t, h, uidAdmin, "GET", "/machines/del-admin/tcp?port=80", ""); rr.Code != http.StatusUpgradeRequired {
		t.Fatalf("sin Upgrade: %d %s", rr.Code, rr.Body)
	}
	if rr := como(t, h, uidNadie, "GET", "/machines/del-admin/tcp?port=80", ""); rr.Code != http.StatusForbidden {
		t.Fatalf("sin rol: %d %s", rr.Code, rr.Body)
	}
	// Un inquilino no llega a la máquina de otro.
	if rr := como(t, h, uidA, "GET", "/machines/caja-b/tcp?port=80", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("máquina ajena: %d %s", rr.Code, rr.Body)
	}
}
