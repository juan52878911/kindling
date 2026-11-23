package frontal

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// cerrojosVivos es cuántas claves tiene el mapa de cerrojos.
func cerrojosVivos(s *Servidor) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cerrojos)
}

// TestCerrojosNoSeAcumulan: cada clave del mapa de cerrojos tiene que irse
// al soltarse el último que la usa. Antes, cada ruta distinta de
// files/replace y cada sandbox con metadatos dejaban una entrada para
// siempre, y el mapa crecía con lo que mandaran los inquilinos.
func TestCerrojosNoSeAcumulan(t *testing.T) {
	f := nuevoFalso(t)
	srv, s := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	url := "/v1/sandboxes/" + sb.ID + "/files/replace"
	for i := range 50 {
		resp := pide(t, srv, tokenAlice, "POST", url, ReemplazarPeticion{Path: fmt.Sprintf("/no/existe/%d", i), Old: "a", New: "b"})
		esperaCodigo(t, resp, 404)
		resp.Body.Close()
	}
	resp := pide(t, srv, tokenAlice, "PATCH", "/v1/sandboxes/"+sb.ID+"/metadata", MetadatosPeticion{Labels: map[string]string{"env": "x"}})
	esperaCodigo(t, resp, 200)
	resp.Body.Close()
	esperaCodigo(t, pide(t, srv, tokenAlice, "DELETE", "/v1/sandboxes/"+sb.ID, nil), 204)
	if n := cerrojosVivos(s); n != 0 {
		t.Errorf("quedan %d cerrojos en el mapa tras soltarlos todos", n)
	}
}

// TestCerrojoExcluye: el cerrojo con recuento sigue excluyendo mientras
// alguien lo tiene, aunque otros esperen y se vayan.
func TestCerrojoExcluye(t *testing.T) {
	s := &Servidor{}
	soltar := s.bloquear("k")
	dentro, fuera := make(chan struct{}), make(chan struct{})
	go func() {
		u := s.bloquear("k")
		close(dentro)
		u()
		close(fuera)
	}()
	select {
	case <-dentro:
		t.Fatal("dos a la vez dentro del mismo cerrojo")
	case <-time.After(50 * time.Millisecond):
	}
	soltar()
	<-fuera
	if n := cerrojosVivos(s); n != 0 {
		t.Errorf("quedan %d cerrojos", n)
	}
}

// TestRutaDemasiadoLarga: una ruta más larga que PATH_MAX no llega al host
// (ningún sistema la acepta) y no sirve para engordar claves internas.
func TestRutaDemasiadoLarga(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	larga := "/" + strings.Repeat("a", 8192)
	resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes/"+sb.ID+"/files/replace", ReemplazarPeticion{Path: larga, Old: "a", New: "b"})
	esperaCodigo(t, resp, 400)
	resp.Body.Close()
}
