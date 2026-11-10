package publicar

import (
	"bufio"
	"context"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestSiguientePaso(t *testing.T) {
	var got []time.Duration
	for p := pasoMarcarMin; len(got) < 7; p = siguientePaso(p, pasoMarcar) {
		got = append(got, p)
	}
	want := []time.Duration{5, 10, 20, 40, 80, 100, 100}
	for i := range want {
		if got[i] != want[i]*time.Millisecond {
			t.Fatalf("pasos = %v, want %v ms", got, want)
		}
	}
}

// Un servicio que empieza a escuchar poco después (lo normal tras un
// deshielo) se alcanza en cuanto escucha, no al cabo de un paso fijo.
func TestServicioQueEmpiezaAEscucharTarde(t *testing.T) {
	puerto := puertoLibre(t)
	f := &maquinasFalsas{ms: map[string]*api.Machine{
		"a": {ID: "a", Name: "a", State: api.StateRunning, Forwards: map[string]string{"80": "127.0.0.1:" + strconv.Itoa(puerto)}, Labels: map[string]string{api.LabelPorts: "80"}},
	}}
	p := New(f, "", t.Logf)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Arrancar(ctx, nil)
	ma, _ := f.Get("a")
	pub, err := p.Publicar(ma, []api.PortPublish{{Host: 0, Guest: 80}})
	if err != nil {
		t.Fatal(err)
	}
	listo := make(chan time.Time, 1)
	lns := make(chan net.Listener, 1)
	go func() {
		time.Sleep(30 * time.Millisecond)
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(puerto))
		if err != nil {
			t.Error(err)
			close(listo)
			return
		}
		lns <- ln
		listo <- time.Now()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					_, _ = io.WriteString(c, "eco:"+sc.Text()+"\n")
				}
			}()
		}
	}()
	r, err := pedir(t, "127.0.0.1:"+strconv.Itoa(pub.Ports[0].Host), "hola")
	fin := time.Now()
	escucha, ok := <-listo
	if !ok {
		t.FailNow()
	}
	defer (<-lns).Close()
	if err != nil || r != "eco:hola" {
		t.Fatalf("pedir = %q, %v", r, err)
	}
	// Con los pasos 5, 10, 20, 40... el reintento que alcanza al servicio
	// llega como mucho ~40 ms después de que escuche; se deja margen para
	// -race y máquinas cargadas, pero por debajo del paso fijo de antes.
	if d := fin.Sub(escucha); d > 90*time.Millisecond {
		t.Errorf("respondió %v después de escuchar, want < 90ms", d)
	} else {
		t.Logf("respondió %v después de escuchar", d)
	}
}

// conSondeo es un gestor que además sabe sondear el puerto (como el de macOS)
// y siempre dice que escucha.
type conSondeo struct {
	*maquinasFalsas
	sondeos atomic.Int32
}

func (c *conSondeo) ProbeGuestPort(context.Context, string, int) (bool, bool) {
	c.sondeos.Add(1)
	return true, true
}

// Si el sondeo dice que escucha pero marcar sigue fallando, se vuelve a
// esperar: nada de un bucle caliente de sondeos y diales hasta el plazo.
func TestSondeoQueMienteNoDaBucleCaliente(t *testing.T) {
	// El 1 nunca escucha: un puerto recién liberado lo puede coger otro test
	// en paralelo y el dial conectaría (fallo intermitente del CI).
	cerrado := 1
	f := &conSondeo{maquinasFalsas: &maquinasFalsas{ms: map[string]*api.Machine{
		"a": {ID: "a", Name: "a", State: api.StateRunning, Forwards: map[string]string{"80": "127.0.0.1:" + strconv.Itoa(cerrado)}, Labels: map[string]string{api.LabelPorts: "80"}},
	}}}
	p := New(f, "", t.Logf)
	p.plazoMarcar = 300 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Arrancar(ctx, nil)
	mq := &maquina{id: "a", nombre: "a"}
	if _, err := p.marcar(mq, 80); err == nil {
		t.Fatal("marcar conectó con un puerto cerrado")
	}
	// 5+10+20+40+80+100+100 ms: unos 7-8 pasos en 300 ms, un sondeo por paso.
	if n := f.sondeos.Load(); n == 0 || n > 12 {
		t.Fatalf("sondeos = %d, want 1..12", n)
	}
}
