package net

import (
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 50 Setup a la vez comparten comprobaciones del host: como mucho la que ya
// corría y una siguiente que cubre al resto.
func TestAsegurarHostInputCincuentaALaVez(t *testing.T) {
	f := &iptablesFalso{}
	usarIptablesFalso(t, f)
	if err := SetupHost(AccesoAgente{UIDs: []int{1000}}); err != nil {
		t.Fatal(err)
	}
	var rondas atomic.Int32
	guardarTablas = func() (string, error) {
		rondas.Add(1)
		// Una comprobación que tarda bastante más que lo que tarda el runner
		// más lento en soltar las 50 goroutines (con 20 ms, en macOS del CI
		// las últimas llegaban con la primera ya acabada).
		time.Sleep(200 * time.Millisecond)
		return f.guardado(), nil
	}
	var wg sync.WaitGroup
	salida := make(chan struct{})
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-salida // todas a la vez
			if err := asegurarHostInput(); err != nil {
				t.Error(err)
			}
		}()
	}
	close(salida)
	wg.Wait()
	if n := rondas.Load(); n < 1 || n > 3 {
		t.Fatalf("%d comprobaciones del host para 50 Setup, quería <= 3", n)
	}
}

// Ventana cero: un Setup que empieza después de borrar una regla nunca se
// queda con el resultado de una comprobación anterior a su llegada.
func TestAsegurarHostInputNoUsaUnaComprobacionAnterior(t *testing.T) {
	f := &iptablesFalso{}
	usarIptablesFalso(t, f)
	if err := SetupHost(AccesoAgente{}); err != nil {
		t.Fatal(err)
	}
	quiere := slices.Clone(f.input)
	empezo, suelta := make(chan struct{}), make(chan struct{})
	var primera sync.Once
	guardarTablas = func() (string, error) {
		foto := f.guardado() // lee ANTES del reload
		primera.Do(func() { close(empezo); <-suelta })
		return foto, nil
	}
	hecho := make(chan error, 1)
	go func() { hecho <- asegurarHostInput() }() // la comprobación en vuelo
	<-empezo
	f.mu.Lock()
	f.input = nil // el reload llega con la comprobación ya leída
	f.mu.Unlock()
	hecho2 := make(chan error, 1)
	go func() { hecho2 <- asegurarHostInput() }() // empieza tras el borrado
	time.Sleep(20 * time.Millisecond)
	close(suelta)
	if err := <-hecho; err != nil {
		t.Fatal(err)
	}
	if err := <-hecho2; err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.input, quiere) {
		t.Fatalf("INPUT = %v, quería %v repuesta", f.input, quiere)
	}
}

// Con todo en su sitio, la instantánea basta: un iptables-save y ningún -C ni
// -S. Si falta una regla, se confirma con el camino lento y se repone.
func TestAsegurarHostInputUnSoloIptablesSave(t *testing.T) {
	f := &iptablesFalso{}
	usarIptablesFalso(t, f)
	if err := SetupHost(AccesoAgente{UIDs: []int{1000}}); err != nil {
		t.Fatal(err)
	}
	var guardados, lentas atomic.Int32
	guardarTablas = func() (string, error) { guardados.Add(1); return f.guardado(), nil }
	comprobarEstadoViejo := comprobarEstado
	comprobarEstado = func(a []string) (bool, error) { lentas.Add(1); return comprobarEstadoViejo(a) }
	listarViejo := listarTabla
	listarTabla = func(t, c string) ([]string, error) { lentas.Add(1); return listarViejo(t, c) }
	if err := asegurarHostInput(); err != nil || guardados.Load() != 1 || lentas.Load() != 0 {
		t.Fatalf("err %v, %d iptables-save, %d comprobaciones lentas; quería nil, 1 y 0", err, guardados.Load(), lentas.Load())
	}
	f.mu.Lock()
	f.input = []string{"-p tcp --dport 22 -j ACCEPT"}
	f.mu.Unlock()
	if err := asegurarHostInput(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.input, "|"); !strings.Contains(got, "DROP") {
		t.Fatalf("INPUT sin reponer: %v", f.input)
	}
}

// restoreRules con --wait: el kernel espera por nosotros, una sola llamada y
// ninguna pausa nuestra.
func TestRestoreConElCerrojoOcupadoNoDuerme(t *testing.T) {
	oldR, oldD := ejecutarRestore, dormir
	t.Cleanup(func() { ejecutarRestore, dormir = oldR, oldD })
	var llamadas, pausas int
	ejecutarRestore = func(ns, entrada string) ([]byte, error) {
		llamadas++
		time.Sleep(30 * time.Millisecond) // iptables-restore esperando el cerrojo
		return nil, nil
	}
	dormir = func(time.Duration) { pausas++ }
	n := &Net{NS: "kl-prueba"}
	if err := n.restoreRules([][]string{{"iptables", "-A", "FORWARD", "-j", "DROP"}}); err != nil || llamadas != 1 || pausas != 0 {
		t.Fatalf("err %v, %d llamadas, %d pausas; quería nil, 1 y 0", err, llamadas, pausas)
	}
}

func TestVersionRestoreConWait(t *testing.T) {
	for v, quiere := range map[string]bool{
		"iptables-restore v1.8.7 (nf_tables)": true,
		"iptables-restore v1.6.2":             true,
		"iptables-restore v1.6.1":             false,
		"iptables-restore v1.4.21":            false,
		"iptables-restore v2.0.0":             true,
		"basura":                              false,
	} {
		if got := versionRestoreConWait(v); got != quiere {
			t.Errorf("%q = %v, quería %v", v, got, quiere)
		}
	}
}

// Ordenar tokens sueltos haría iguales dos reglas con origen y destino
// cambiados; ordenando cláusulas no.
func TestNormalizarReglaNoMezclaValores(t *testing.T) {
	a := normalizarRegla(strings.Fields("-s 10.0.0.1 -d 10.0.0.2 -j DROP"))
	b := normalizarRegla(strings.Fields("-s 10.0.0.2 -d 10.0.0.1 -j DROP"))
	if slices.Equal(a, b) {
		t.Fatalf("reglas distintas normalizadas igual: %v", a)
	}
	c := normalizarRegla(strings.Fields("-d 10.0.0.2 -s 10.0.0.1 -j DROP"))
	if !slices.Equal(a, c) {
		t.Fatalf("mismo orden lógico normalizado distinto: %v vs %v", a, c)
	}
	n1 := normalizarRegla(strings.Fields("! -s 10.0.0.1 -d 10.0.0.2 -j DROP"))
	n2 := normalizarRegla(strings.Fields("-s 10.0.0.1 ! -d 10.0.0.2 -j DROP"))
	if slices.Equal(n1, n2) {
		t.Fatalf("negación movida normalizada igual: %v", n1)
	}
}
