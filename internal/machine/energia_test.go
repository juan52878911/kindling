package machine

import (
	"bufio"
	"context"
	"io"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
)

// El vigía avisa de que el Mac se va a dormir: se pausan las que corren
// (no las que ya estaban en pausa), se contesta, y al despertar se reanudan
// solo esas. Si el vigía muere entre medias, se reanudan igual.
func TestEnergiaPausaAntesDeDormir(t *testing.T) {
	m := newTestManager(t)
	b := nuevoBancoBateria(t, m, BatteryConfig{Mode: config.BatteryOff})
	maquinaBateria(m, "a", nil)
	maquinaBateria(m, "b", nil)
	maquinaBateria(m, "ya-en-pausa", nil)
	m.mu.Lock()
	m.byID["ya-en-pausa"].State = api.StatePaused
	m.mu.Unlock()

	daemonR, vigiaW := io.Pipe() // lo que dice el vigía
	vigiaR, daemonW := io.Pipe() // lo que contesta el daemon
	fin := make(chan struct{})
	go func() { m.atenderEnergia(context.Background(), daemonR, daemonW); close(fin) }()
	resp := bufio.NewScanner(vigiaR)
	dice := func(s string) {
		t.Helper()
		if _, err := io.WriteString(vigiaW, s+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	dice("ready")
	dice("sleep 42")
	if !resp.Scan() || resp.Text() != "ack 42" {
		t.Fatalf("respuesta %q", resp.Text())
	}
	b.mu.Lock()
	pausadas := slices.Sorted(slices.Values(b.pausadas))
	b.mu.Unlock()
	if !slices.Equal(pausadas, []string{"a", "b"}) {
		t.Fatalf("pausadas %v", pausadas)
	}
	dice("wake")
	espera := time.Now().Add(3 * time.Second)
	for {
		b.mu.Lock()
		n := len(b.reanud)
		b.mu.Unlock()
		if n == 2 || time.Now().After(espera) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	b.mu.Lock()
	reanud := slices.Sorted(slices.Values(b.reanud))
	b.mu.Unlock()
	if !slices.Equal(reanud, []string{"a", "b"}) {
		t.Fatalf("reanudadas %v", reanud)
	}
	if mc, _ := m.Get("ya-en-pausa"); mc.State != api.StatePaused {
		t.Fatal("reanudó una que no pausó")
	}

	// Otro sueño y el vigía muere sin despertar: se reanudan al salir.
	dice("sleep 43")
	if !resp.Scan() || resp.Text() != "ack 43" {
		t.Fatalf("respuesta %q", resp.Text())
	}
	vigiaW.Close()
	<-fin
	b.mu.Lock()
	n := len(b.reanud)
	b.mu.Unlock()
	if n != 4 {
		t.Fatalf("tras morir el vigía se reanudaron %d en total, quería 4", n)
	}
}

// El vigía dice de dónde come el Mac al arrancar y en cada cambio: el daemon
// se lo cree en el acto y, mientras el vigía viva, no pregunta a pmset. Si el
// vigía muere, vuelve a preguntar él.
func TestEnergiaFuenteDelVigia(t *testing.T) {
	m := newTestManager(t)
	b := nuevoBancoBateria(t, m, BatteryConfig{Mode: config.BatteryAuto})
	b.mu.Lock()
	b.enBat = false
	b.mu.Unlock()
	t.Cleanup(func() { fuenteVigia.Store(fuenteNoSe) })
	if m.EnBateria() {
		t.Fatal("con corriente, en batería")
	}

	daemonR, vigiaW := io.Pipe()
	fin := make(chan struct{})
	go func() { m.atenderEnergia(context.Background(), daemonR, io.Discard); close(fin) }()
	dice := func(s string) {
		t.Helper()
		if _, err := io.WriteString(vigiaW, s+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	esperar := func(quiero bool) {
		t.Helper()
		limite := time.Now().Add(3 * time.Second)
		for m.EnBateria() != quiero {
			if time.Now().After(limite) {
				t.Fatalf("EnBateria() no pasó a %v", quiero)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	dice("ready")
	dice("power battery")
	esperar(true) // sin esperar a que caduque la lectura de hace un momento
	b.mu.Lock()
	lecturas := b.lecturas
	b.mu.Unlock()
	if lecturas != 1 {
		t.Fatalf("%d lecturas de la fuente; el aviso del vigía no debía preguntar", lecturas)
	}
	if runtime.GOOS == "darwin" {
		// Sin el punto de prueba, lo que lee el daemon es enBateriaSO: con el
		// vigía vivo, lo que dijo él (este Mac puede estar enchufado).
		if v, err := enBateriaSO(); err != nil || !v {
			t.Fatalf("enBateriaSO() = %v, %v; quería lo del vigía (batería)", v, err)
		}
	}
	dice("power ac")
	esperar(false)

	vigiaW.Close()
	<-fin
	if v := fuenteVigia.Load(); v != fuenteNoSe {
		t.Fatalf("muerto el vigía, la fuente sigue en %d; quería que se volviera a preguntar", v)
	}
}
