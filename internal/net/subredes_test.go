package net

import (
	"fmt"
	stdnet "net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// hostFalso es un host compartido por varios daemons: las direcciones que sus
// Setup ponen en los veth.
type hostFalso struct {
	mu   sync.Mutex
	dirs []DireccionHost
}

func (h *hostFalso) poner(n *Net) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dirs = append(h.dirs, DireccionHost{If: n.HostIf, IP: stdnet.ParseIP(n.HostIP).To4()})
}

func (h *hostFalso) listar() ([]DireccionHost, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]DireccionHost(nil), h.dirs...), nil
}

func conHostFalso(t *testing.T) *hostFalso {
	t.Helper()
	if !redEnHost {
		t.Skip("en macOS no hay enlaces en el host")
	}
	h := &hostFalso{}
	antesD, antesR := DireccionesHost, DirReservas
	t.Cleanup(func() { DireccionesHost, DirReservas = antesD, antesR })
	DireccionesHost = h.listar
	DirReservas = filepath.Join(t.TempDir(), "net-claims")
	return h
}

// daemonFalso reparte índices como Manager: su cursor y sus máquinas.
type daemonFalso struct {
	mu     sync.Mutex
	cursor int
	suyos  map[int]bool
}

func (d *daemonFalso) asignar(t *testing.T, id string) *Net {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	n, err := Asignar(&d.cursor, 16000, func(i int) bool { return d.suyos[i] }, id)
	if err != nil {
		t.Fatal(err)
	}
	d.suyos[n.Index] = true
	return n
}

// El choque del lab: el daemon del sistema y uno privado arrancan cada uno su
// primera máquina. Los dos empezaban en el índice 1 y montaban la misma /30.
func TestDosDaemonsNoRepitenSubred(t *testing.T) {
	h := conHostFalso(t)
	sistema := &daemonFalso{suyos: map[int]bool{}}
	privado := &daemonFalso{suyos: map[int]bool{}}

	a := sistema.asignar(t, "171ae619aaaa")
	h.poner(a) // Setup
	a.SoltarReserva()

	b := privado.asignar(t, "0000beef0000")
	h.poner(b)
	b.SoltarReserva()

	if a.Index == b.Index || a.HostIP == b.HostIP {
		t.Fatalf("los dos daemons montaron la misma subred: %s (%d) y %s (%d)", a.HostIP, a.Index, b.HostIP, b.Index)
	}
}

// Y a la vez: muchos arranques simultáneos en dos daemons, con la reserva
// entre elegir el índice y poner la dirección. Ningún índice se repite.
func TestDosDaemonsALaVez(t *testing.T) {
	h := conHostFalso(t)
	daemons := []*daemonFalso{{suyos: map[int]bool{}}, {suyos: map[int]bool{}}}
	var mu sync.Mutex
	vistos := map[int]string{}
	var wg sync.WaitGroup
	for d := range daemons {
		for i := 0; i < 40; i++ {
			wg.Add(1)
			go func(d, i int) {
				defer wg.Done()
				id := fmt.Sprintf("%02x%06x", d, i)
				n := daemons[d].asignar(t, id)
				time.Sleep(time.Millisecond) // lo que tarda Setup
				h.poner(n)
				n.SoltarReserva()
				mu.Lock()
				defer mu.Unlock()
				if otro, ok := vistos[n.Index]; ok {
					t.Errorf("índice %d repetido: %s y %s", n.Index, otro, id)
				}
				vistos[n.Index] = id
			}(d, i)
		}
	}
	wg.Wait()
}

// Una reserva viva de otro daemon aparta el índice; una caducada (daemon
// muerto a medias) no.
func TestReservaDeOtroYCaducada(t *testing.T) {
	conHostFalso(t)
	if err := os.MkdirAll(DirReservas, 0o755); err != nil {
		t.Fatal(err)
	}
	viva := filepath.Join(DirReservas, "1")
	if err := os.WriteFile(viva, []byte("1 kl-otro\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Plan(1, "aaaaaaaa").Reservar() {
		t.Fatal("el índice que otro daemon está montando no se reserva")
	}
	vieja := time.Now().Add(-2 * reservaCaduca)
	if err := os.Chtimes(viva, vieja, vieja); err != nil {
		t.Fatal(err)
	}
	n := Plan(1, "aaaaaaaa")
	if !n.Reservar() {
		t.Fatal("una reserva caducada no aparta el índice")
	}
	n.SoltarReserva()
	if _, err := os.Stat(viva); !os.IsNotExist(err) {
		t.Fatal("SoltarReserva borra el fichero")
	}
}

// La dirección del propio veth (red que se rehace) no ocupa su índice; la de
// otra interfaz, sí.
func TestReservarIgnoraElVethPropio(t *testing.T) {
	h := conHostFalso(t)
	n := Plan(7, "cafecafe")
	h.poner(n)
	if !n.Reservar() {
		t.Fatal("la dirección del propio veth no ocupa el índice")
	}
	n.SoltarReserva()
	otro := Plan(7, "0badf00d")
	if otro.Reservar() {
		t.Fatal("una /30 con la dirección del veth de otra máquina está ocupada")
	}
}

func TestIndiceDe(t *testing.T) {
	for _, idx := range []int{1, 63, 64, 1000, 15999} {
		n := Plan(idx, "x")
		if got := IndiceDe(stdnet.ParseIP(n.HostIP)); got != idx {
			t.Errorf("IndiceDe(%s) = %d, quiero %d", n.HostIP, got, idx)
		}
	}
	if IndiceDe(stdnet.ParseIP("10.0.0.1")) != 0 || IndiceDe(stdnet.ParseIP("172.31.0.1")) != 0 {
		t.Error("fuera de 172.30.0.0/16 no hay índice")
	}
}
