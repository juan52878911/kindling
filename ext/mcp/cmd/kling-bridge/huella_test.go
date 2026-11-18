package main

import (
	"errors"
	"net/http"
	"testing"

	"github.com/juan52878911/kindling/pkg/guest"
)

// La huella del entorno de un mismo almacén MMDS no puede depender del orden en
// que Go recorre un map: si cambia, ensureProxyChild relanza el servidor HTTP
// compartido sin que haya cambiado ningún secreto.
func TestHuellaEstableConVariosSecretos(t *testing.T) {
	st := &guest.MMDSStore{Env: map[string]string{"A": "1", "B": "2", "C": "3"}}
	conMMDS(t, &st)
	b := &bridge{env: []string{"X=1"}}
	e0, _ := b.sessionEnv("http-mode")
	h0 := huellaEnv(e0)
	dist := 0
	for i := 0; i < 200; i++ {
		e, _ := b.sessionEnv("http-mode")
		if huellaEnv(e) != h0 {
			dist++
		}
	}
	if dist > 0 {
		t.Fatalf("la huella del mismo almacén MMDS cambió en %d de 200 lecturas", dist)
	}
}

// Con el MMDS intacto, los POST sin sesión no relanzan el hijo HTTP.
func TestProxyNoRelanzaConMismosSecretos(t *testing.T) {
	st := &guest.MMDSStore{Env: map[string]string{"A": "1", "B": "2", "C": "3"}}
	conMMDS(t, &st)
	b := puenteProxy(t)
	p0 := pedir(t, b, http.MethodPost)
	cambios := 0
	for i := 0; i < 6; i++ {
		if p := pedir(t, b, http.MethodPost); p != p0 {
			cambios++
			p0 = p
		}
	}
	if cambios > 0 {
		t.Fatalf("el hijo HTTP compartido se relanzó %d veces en 6 POST sin sesión con el MMDS intacto", cambios)
	}
}

// Si MMDS deja de contestar, el hijo sigue con los secretos que tenía: antes se
// relanzaba con el entorno base, sin ninguno.
func TestProxyNoRelanzaSiMMDSFalla(t *testing.T) {
	st := &guest.MMDSStore{Env: map[string]string{"A": "1"}}
	conMMDS(t, &st)
	b := puenteProxy(t)
	p0 := pedir(t, b, http.MethodPost)

	fetchMMDS = func() (*guest.MMDSStore, error) { return nil, errors.New("sin respuesta") }
	if p := pedir(t, b, http.MethodPost); p != p0 {
		t.Fatalf("con MMDS caído el hijo se relanzó: antes %q, después %q", p0, p)
	}
}
