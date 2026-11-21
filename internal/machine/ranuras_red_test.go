//go:build !darwin

package machine

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	knet "github.com/juan52878911/kindling/internal/net"
)

// fondoRanurasDePrueba registra en m un fondo con ranuras de mentira (sin procesos).
func fondoRanurasDePrueba(t *testing.T, m *Manager, tam int) *knet.Fondo {
	t.Helper()
	var i atomic.Int64
	f := knet.NuevoFondo(tam, func() (*knet.Ranura, error) {
		k := int(i.Add(1))
		id := fmt.Sprintf("%08x00000000", 0xa0000000+k)
		return &knet.Ranura{ID: id, Net: knet.Plan(k, id)}, nil
	}, func(*knet.Ranura) {})
	m.fondoRed.once.Do(func() {})
	m.fondoRed.f.Store(f)
	t.Cleanup(func() { m.fondoRed.f.Store(nil); f.Cerrar() })
	f.Reponer()
	for n := 0; n < 2000 && f.Len() < tam; n++ {
		time.Sleep(time.Millisecond)
	}
	if f.Len() < tam {
		t.Fatalf("el fondo tiene %d ranuras, quería %d", f.Len(), tam)
	}
	return f
}

func TestBarrerNamespacesNoBorraRanurasEnEsperaSiLasDeUnDaemonAnterior(t *testing.T) {
	m := &Manager{root: t.TempDir()}
	// Un daemon anterior dejó su ranura (con marca) y murió.
	m.marcarRedPropia("kl-deadbeef")
	f := fondoRanurasDePrueba(t, m, 2)
	enEspera := knet.Plan(1, "a0000001").NS
	m.marcarRedPropia(enEspera)
	if !f.Propia(enEspera) {
		t.Fatalf("el fondo no reconoce %s", enEspera)
	}

	var borrados []string
	antesL, antesB := listarNamespaces, borrarNamespace
	t.Cleanup(func() { listarNamespaces, borrarNamespace = antesL, antesB })
	borrarNamespace = func(ns string) { borrados = append(borrados, ns) }
	listarNamespaces = func() []string { return []string{enEspera, "kl-deadbeef"} }

	m.barrerNamespaces(map[string]bool{})
	if len(borrados) != 1 || borrados[0] != "kl-deadbeef" {
		t.Fatalf("borrados = %v, quería solo la ranura del daemon anterior", borrados)
	}
	if !m.redEsPropia(enEspera) {
		t.Fatal("el barrido quitó la marca de una ranura en espera")
	}
}

func TestNuevoIDYAsignarRedUsanLaRanura(t *testing.T) {
	m := &Manager{root: t.TempDir(), byID: nil}
	fondoRanurasDePrueba(t, m, 2)
	id := m.nuevoIDMaquina()
	n, err := m.asignarRed(id)
	if err != nil {
		t.Fatal(err)
	}
	if n.NS != "kl-"+knet.Corto(id) || n.Index == 0 {
		t.Fatalf("asignarRed no devolvió la red de la ranura: %+v", n)
	}
}

func TestSinFondoNuevoIDEsElDeSiempre(t *testing.T) {
	t.Setenv("KLING_NET_POOL", "0")
	m := &Manager{root: t.TempDir()}
	id := m.nuevoIDMaquina()
	if len(id) != 16 {
		t.Fatalf("id %q", id)
	}
	if m.ranuraEntregar(id) != nil {
		t.Fatal("con el fondo apagado no hay ranuras")
	}
}
