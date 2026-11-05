package gateway

import (
	"fmt"
	"strings"
	"testing"
)

// La memoria de uso no pasa de maxHits consultas: olvida la de uso más
// antiguo y conserva las que se siguen usando.
func TestMemoriaAcotada(t *testing.T) {
	m := newMemory(nil, "mem")
	m.note("leer fichero", "fs.read")
	for i := 0; i < maxHits+100; i++ {
		m.note(fmt.Sprintf("consulta numero %d", i), "otra.tool")
		if i%100 == 0 {
			m.note("leer fichero", "fs.read") // se sigue usando
		}
	}
	if n := len(m.hits); n != maxHits {
		t.Fatalf("%d consultas recordadas; tope %d", n, maxHits)
	}
	if _, ok := m.hits["consulta numero 0"]; ok {
		t.Error("la consulta más antigua debía olvidarse")
	}
	if e := m.hits["leer fichero"]; e == nil || e.byTool["fs.read"] == 0 {
		t.Error("la consulta en uso no debía olvidarse")
	}
	got := m.Rank("leer fichero grande", []Tool{{Qualified: "otra.tool"}, {Qualified: "fs.read"}})
	if got[0].Qualified != "fs.read" {
		t.Errorf("Rank puso delante %q", got[0].Qualified)
	}
}

// Una consulta enorme se guarda recortada.
func TestMemoriaRecortaConsultas(t *testing.T) {
	m := newMemory(nil, "mem")
	m.note(strings.Repeat("x", 1<<20), "t")
	for k := range m.hits {
		if len(k) > maxQueryLen+len("…") {
			t.Fatalf("clave de %d bytes", len(k))
		}
	}
}
