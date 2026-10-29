package publicar

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// maquinasFalsas es un gestor con máquinas cuyo "invitado" es un servidor de
// este proceso: Forwards lleva cada puerto a su dirección, como en el Mac.
type maquinasFalsas struct {
	mu       sync.Mutex
	ms       map[string]*api.Machine
	thaws    int
	freezes  []string
	gfreezes []string
	alThaw   func(mc *api.Machine) // cambia el reenvío al descongelar
}

func (f *maquinasFalsas) Get(ref string) (*api.Machine, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mc, ok := f.ms[ref]
	if !ok {
		return nil, false
	}
	c := *mc
	return &c, true
}

func (f *maquinasFalsas) List() []*api.Machine {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*api.Machine
	for _, mc := range f.ms {
		c := *mc
		out = append(out, &c)
	}
	return out
}

func (f *maquinasFalsas) Thaw(_ context.Context, ref string) (*api.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mc := f.ms[ref]
	f.thaws++
	mc.State = api.StateRunning
	if f.alThaw != nil {
		f.alThaw(mc)
	}
	c := *mc
	return &c, nil
}

func (f *maquinasFalsas) Freeze(_ context.Context, ref string) (*api.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.freezes = append(f.freezes, ref)
	f.ms[ref].State = api.StateWarm
	c := *f.ms[ref]
	return &c, nil
}

func (f *maquinasFalsas) GraphFreeze(_ context.Context, ref string) (*api.Graph, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gfreezes = append(f.gfreezes, ref)
	for _, mc := range f.ms {
		if mc.Labels[api.LabelGraph] == ref {
			mc.State = api.StateWarm
		}
	}
	return &api.Graph{}, nil
}

// eco es un servidor TCP que devuelve "eco:" + cada línea.
func eco(t *testing.T) net.Listener {
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
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					io.WriteString(c, "eco:"+sc.Text()+"\n")
				}
			}()
		}
	}()
	return ln
}

func pedir(t *testing.T, addr, linea string) (string, error) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, linea+"\n"); err != nil {
		return "", err
	}
	r, err := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(r), err
}

func puertoLibre(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func nuevo(t *testing.T, f *maquinasFalsas) *Publicador {
	t.Helper()
	p := New(f, filepath.Join(t.TempDir(), "publish.json"), t.Logf)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p.Arrancar(ctx, nil)
	return p
}

// Publicar lleva 127.0.0.1:H al puerto del invitado, resolviendo el reenvío
// en cada conexión, y despierta la máquina congelada.
func TestPublicarYDespertar(t *testing.T) {
	srv := eco(t)
	f := &maquinasFalsas{ms: map[string]*api.Machine{
		"m1": {ID: "m1", Name: "db", State: api.StateWarm, Labels: map[string]string{api.LabelPorts: "5432"}},
	}}
	// Congelada no tiene reenvío; al despertar lo tiene (y cambia).
	f.alThaw = func(mc *api.Machine) { mc.Forwards = map[string]string{"5432": srv.Addr().String()} }
	p := nuevo(t, f)
	mc, _ := f.Get("m1")
	pub, err := p.Publicar(mc, []api.PortPublish{{Host: 0, Guest: 5432}})
	if err != nil {
		t.Fatal(err)
	}
	h := pub.Ports[0].Host
	if h == 0 {
		t.Fatal("Host 0 no recibió un puerto")
	}
	got, err := pedir(t, "127.0.0.1:"+strconv.Itoa(h), "hola")
	if err != nil || got != "eco:hola" {
		t.Fatalf("got %q, %v", got, err)
	}
	if f.thaws != 1 {
		t.Fatalf("thaws = %d, want 1", f.thaws)
	}
	if l := p.Lista(); len(l) != 1 || l[0].Name != "db" || l[0].Ports[0].Host != h {
		t.Fatalf("Lista = %+v", l)
	}
	if !p.Quitar("m1") {
		t.Fatal("Quitar: nada")
	}
	if _, err := pedir(t, "127.0.0.1:"+strconv.Itoa(h), "x"); err == nil {
		t.Fatal("sigue publicado tras Quitar")
	}
}

// Un puerto ocupado (por otra máquina o por otro proceso) falla y no deja
// publicado ninguno de los demás.
func TestPublicarTodoONada(t *testing.T) {
	srv := eco(t)
	fw := map[string]string{"80": srv.Addr().String(), "81": srv.Addr().String()}
	f := &maquinasFalsas{ms: map[string]*api.Machine{
		"a": {ID: "a", Name: "a", State: api.StateRunning, Forwards: fw, Labels: map[string]string{api.LabelPorts: "80,81"}},
		"b": {ID: "b", Name: "b", State: api.StateRunning, Forwards: fw, Labels: map[string]string{api.LabelPorts: "80,81"}},
	}}
	p := nuevo(t, f)
	ma, _ := f.Get("a")
	mb, _ := f.Get("b")
	h1, h2 := puertoLibre(t), puertoLibre(t)
	if _, err := p.Publicar(ma, []api.PortPublish{{Host: h1, Guest: 80}}); err != nil {
		t.Fatal(err)
	}
	_, err := p.Publicar(mb, []api.PortPublish{{Host: h2, Guest: 81}, {Host: h1, Guest: 80}})
	if !errors.Is(err, ErrOcupado) || !strings.Contains(err.Error(), "published for a") {
		t.Fatalf("err = %v", err)
	}
	if p.De("b") != nil {
		t.Fatalf("b quedó con %v", p.De("b"))
	}
	if _, err := pedir(t, "127.0.0.1:"+strconv.Itoa(h2), "x"); err == nil {
		t.Fatal("h2 quedó publicado")
	}
	// Ocupado por otro proceso.
	otro, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer otro.Close()
	if _, err := p.Publicar(mb, []api.PortPublish{{Host: otro.Addr().(*net.TCPAddr).Port, Guest: 81}}); !errors.Is(err, ErrOcupado) {
		t.Fatalf("err = %v", err)
	}
	// En el Mac, un puerto que la máquina no expone no se puede alcanzar.
	if _, err := p.Publicar(mb, []api.PortPublish{{Host: 0, Guest: 9999}}); err == nil || !strings.Contains(err.Error(), "not exposed") {
		t.Fatalf("err = %v", err)
	}
	// Ni UDP, que el VMM del Mac no reenvía.
	if _, err := p.Publicar(mb, []api.PortPublish{{Host: 0, Guest: 81, Proto: "udp"}}); !errors.Is(err, ErrNoSoportado) {
		t.Fatalf("err = %v", err)
	}
	if _, err := p.Publicar(mb, []api.PortPublish{{Host: 0, Guest: api.GuestPort}}); err == nil {
		t.Fatal("publicó el puerto del agente")
	}
}

// Las publicaciones se guardan y se recuperan al arrancar otro publicador
// (el daemon reiniciado); las de máquinas que ya no existen, no.
func TestPublicarSobreviveAlReinicio(t *testing.T) {
	srv := eco(t)
	fw := map[string]string{"80": srv.Addr().String()}
	f := &maquinasFalsas{ms: map[string]*api.Machine{
		"a": {ID: "a", Name: "a", State: api.StateRunning, Forwards: fw, Labels: map[string]string{api.LabelPorts: "80"}},
		"b": {ID: "b", Name: "b", State: api.StateRunning, Forwards: fw, Labels: map[string]string{api.LabelPorts: "80"}},
	}}
	ruta := filepath.Join(t.TempDir(), "publish.json")
	p1 := New(f, ruta, t.Logf)
	ctx1, cancel1 := context.WithCancel(context.Background())
	p1.Arrancar(ctx1, nil)
	ma, _ := f.Get("a")
	mb, _ := f.Get("b")
	ha, hb := puertoLibre(t), puertoLibre(t)
	if _, err := p1.Publicar(ma, []api.PortPublish{{Host: ha, Guest: 80}}); err != nil {
		t.Fatal(err)
	}
	if _, err := p1.Publicar(mb, []api.PortPublish{{Host: hb, Guest: 80}}); err != nil {
		t.Fatal(err)
	}
	p1.guardar()
	cancel1()
	// El daemon se va: los puertos se cierran.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := pedir(t, "127.0.0.1:"+strconv.Itoa(ha), "x"); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("el puerto sigue abierto con el publicador parado")
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.mu.Lock()
	delete(f.ms, "b")
	f.mu.Unlock()
	p2 := New(f, ruta, t.Logf)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	p2.Arrancar(ctx2, nil)
	if got, err := pedir(t, "127.0.0.1:"+strconv.Itoa(ha), "otra vez"); err != nil || got != "eco:otra vez" {
		t.Fatalf("tras reiniciar: %q, %v", got, err)
	}
	if l := p2.Lista(); len(l) != 1 || l[0].Machine != "a" {
		t.Fatalf("Lista = %+v", l)
	}
}

// Una máquina borrada deja de publicar sola (el barrido).
func TestBarridoQuitaLasDeMaquinasIdas(t *testing.T) {
	srv := eco(t)
	f := &maquinasFalsas{ms: map[string]*api.Machine{
		"a": {ID: "a", Name: "a", State: api.StateRunning, Forwards: map[string]string{"80": srv.Addr().String()}, Labels: map[string]string{api.LabelPorts: "80"}},
	}}
	p := New(f, "", t.Logf)
	p.barrido = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Arrancar(ctx, nil)
	ma, _ := f.Get("a")
	if _, err := p.Publicar(ma, []api.PortPublish{{Host: 0, Guest: 80}}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	delete(f.ms, "a")
	f.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for len(p.Lista()) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("el barrido no quitó la publicación")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Un servicio que no escucha: quien conecta recibe un RST cuando vence el
// plazo, no un cierre limpio.
func TestServicioQueNoEscuchaDaRST(t *testing.T) {
	cerrado := puertoLibre(t)
	f := &maquinasFalsas{ms: map[string]*api.Machine{
		"a": {ID: "a", Name: "a", State: api.StateRunning, Forwards: map[string]string{"80": "127.0.0.1:" + strconv.Itoa(cerrado)}, Labels: map[string]string{api.LabelPorts: "80"}},
	}}
	p := New(f, "", t.Logf)
	p.plazoMarcar = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Arrancar(ctx, nil)
	ma, _ := f.Get("a")
	pub, err := p.Publicar(ma, []api.PortPublish{{Host: 0, Guest: 80}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(pub.Ports[0].Host))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, err = c.Read(make([]byte, 1))
	if err == nil || !strings.Contains(err.Error(), "reset") {
		t.Fatalf("read = %v, want connection reset", err)
	}
}

// UDP: cada cliente tiene su flujo hacia el invitado (Linux: su IP).
func TestPublicarUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		b := make([]byte, 1500)
		for {
			n, de, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			pc.WriteTo(append([]byte("eco:"), b[:n]...), de)
		}
	}()
	guest := pc.LocalAddr().(*net.UDPAddr).Port
	f := &maquinasFalsas{ms: map[string]*api.Machine{
		"a": {ID: "a", Name: "dns", State: api.StateRunning, IP: "127.0.0.1"},
	}}
	p := nuevo(t, f)
	ma, _ := f.Get("a")
	pub, err := p.Publicar(ma, []api.PortPublish{{Host: 0, Guest: guest, Proto: "udp"}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(pub.Ports[0].Host))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write([]byte("hola"))
	b := make([]byte, 100)
	n, err := c.Read(b)
	if err != nil || string(b[:n]) != "eco:hola" {
		t.Fatalf("got %q, %v", b[:n], err)
	}
	if s := api.FormatPorts(pub.Ports); !strings.HasSuffix(s, "/udp") {
		t.Fatalf("FormatPorts = %q", s)
	}
}

// idle_freeze: un grafo cuyas máquinas publicadas llevan el plazo sin
// conexiones se congela entero; kling.idle=keep, una petición en curso o una
// sesión de arista lo impiden; la siguiente conexión lo despierta.
func TestCongelarOciosas(t *testing.T) {
	srv := eco(t)
	fw := map[string]string{"5432": srv.Addr().String()}
	f := &maquinasFalsas{ms: map[string]*api.Machine{
		"db":    {ID: "db", Name: "p-db", State: api.StateRunning, Forwards: fw, Labels: map[string]string{api.LabelPorts: "5432", api.LabelGraph: "g1"}},
		"cache": {ID: "cache", Name: "p-cache", State: api.StateRunning, Labels: map[string]string{api.LabelGraph: "g1"}},
		"solo":  {ID: "solo", Name: "solo", State: api.StateRunning, Forwards: fw, Labels: map[string]string{api.LabelPorts: "5432", api.LabelIdle: "keep"}},
		"otra":  {ID: "otra", Name: "otra", State: api.StateRunning},
	}}
	f.alThaw = func(mc *api.Machine) {}
	var reloj atomic.Int64
	reloj.Store(time.Now().UnixNano())
	p := New(f, "", t.Logf)
	p.ahora = func() time.Time { return time.Unix(0, reloj.Load()) }
	var mu sync.Mutex
	act := map[string]Actividad{}
	p.SetActividad(func(id string) Actividad { mu.Lock(); defer mu.Unlock(); return act[id] })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Arrancar(ctx, nil)
	db, _ := f.Get("db")
	solo, _ := f.Get("solo")
	pub, err := p.Publicar(db, []api.PortPublish{{Host: 0, Guest: 5432}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Publicar(solo, []api.PortPublish{{Host: 0, Guest: 5432}}); err != nil {
		t.Fatal(err)
	}
	plazo := 10 * time.Minute
	p.congelarOciosas(ctx, plazo)
	if len(f.gfreezes)+len(f.freezes) != 0 {
		t.Fatal("congeló antes del plazo")
	}
	// Pasado el plazo, pero el cache del grafo tiene una sesión de arista.
	reloj.Add(int64(plazo + time.Second))
	ahora := p.ahora()
	mu.Lock()
	act["cache"] = Actividad{Sesiones: 1}
	mu.Unlock()
	p.congelarOciosas(ctx, plazo)
	if len(f.gfreezes) != 0 {
		t.Fatal("congeló con una sesión de arista viva")
	}
	mu.Lock()
	act["cache"] = Actividad{Ultima: ahora.Add(-plazo - time.Second)}
	mu.Unlock()
	p.congelarOciosas(ctx, plazo)
	if len(f.gfreezes) != 1 || f.gfreezes[0] != "g1" || len(f.freezes) != 0 {
		t.Fatalf("gfreezes = %v, freezes = %v (solo lleva kling.idle=keep)", f.gfreezes, f.freezes)
	}
	// La siguiente conexión lo despierta.
	if got, err := pedir(t, "127.0.0.1:"+strconv.Itoa(pub.Ports[0].Host), "x"); err != nil || got != "eco:x" {
		t.Fatalf("tras congelar: %q, %v", got, err)
	}
	if f.thaws != 1 {
		t.Fatalf("thaws = %d", f.thaws)
	}
	// Recién usada: no se congela otra vez en seguida.
	p.congelarOciosas(ctx, plazo)
	if len(f.gfreezes) != 1 {
		t.Fatal("volvió a congelar una recién usada")
	}
}
