package frontal

import (
	"bytes"
	"strings"
	"testing"
)

// files/replace es para editar texto: un fichero de más de maxReemplazar no
// se baja al gateway. Antes el tope era el de subida (64 MiB) y cada
// reemplazo podía retener unas tres copias del fichero en memoria.
func TestReemplazarTopeDeEdicion(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	ref := sb.ID[len("a/"):]
	f.mu.Lock()
	f.ficheros[ref+":/datos"] = []byte("host=a\n")
	f.tamanoStat = maxReemplazar + 1
	f.mu.Unlock()
	resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes/"+sb.ID+"/files/replace", ReemplazarPeticion{Path: "/datos", Old: "host", New: "x"})
	esperaCodigo(t, resp, 413)
	resp.Body.Close()
}

// El resultado tampoco pasa de maxReemplazar, y se mide antes de
// construirlo.
func TestReemplazarResultadoDemasiadoGrande(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	ref := sb.ID[len("a/"):]
	f.mu.Lock()
	f.ficheros[ref+":/datos"] = bytes.Repeat([]byte("a"), 1024)
	f.mu.Unlock()
	nuevo := strings.Repeat("x", 10<<10) // 1024 × 10 KiB = 10 MiB
	resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes/"+sb.ID+"/files/replace", ReemplazarPeticion{Path: "/datos", Old: "a", New: nuevo, All: true})
	esperaCodigo(t, resp, 413)
	resp.Body.Close()
}

// Cada tenant tiene un cupo de cargas de fichero en memoria a la vez; al
// soltarlas, el cupo se recupera y no queda rastro en el mapa.
func TestCupoDeCargasPorTenant(t *testing.T) {
	s := &Servidor{}
	var salidas []func()
	for i := range maxCargasPorTenant {
		salir, ok := s.entrarCarga("alice")
		if !ok {
			t.Fatalf("la carga %d no entró", i)
		}
		salidas = append(salidas, salir)
	}
	if _, ok := s.entrarCarga("alice"); ok {
		t.Fatal("entró una carga por encima del cupo")
	}
	if salir, ok := s.entrarCarga("bob"); !ok {
		t.Fatal("el cupo de alice frenó a bob")
	} else {
		salir()
	}
	for _, salir := range salidas {
		salir()
	}
	if salir, ok := s.entrarCarga("alice"); !ok {
		t.Fatal("tras soltar, alice no recupera el cupo")
	} else {
		salir()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cargas) != 0 {
		t.Errorf("quedan %d entradas en el mapa de cargas", len(s.cargas))
	}
}
