package frontal

import (
	"fmt"
	"testing"
)

// El tope MaxVolumenes también cuenta los clones pedidos al crear un sandbox
// (volumes[].clone_from): con tope 1 el segundo clon da 429 y no deja volumen.
func TestR6ClonAlCrearSaltaMaxVolumenes(t *testing.T) {
	b := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"b": b},
		Tenant{Nombre: "alice", Token: tokenAlice, MaxVolumenes: 1})
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/volumes", CrearVolumenPeticion{Name: "base1", SizeMiB: 128}), 201)
	// El camino directo respeta el tope.
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/volumes", CrearVolumenPeticion{Name: "dos"}), 429)
	for i := 0; i < 5; i++ {
		resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base",
			Volumes: []VolumenPeticion{{Name: fmt.Sprintf("copia%d", i), Mount: "/data", CloneFrom: "base1"}}})
		if resp.StatusCode == 429 {
			l := decodifica[[]Volumen](t, pide(t, srv, tokenAlice, "GET", "/v1/volumes", nil))
			if len(l) != 1 {
				t.Fatalf("el 429 dejó %d volúmenes, quería 1", len(l))
			}
			return
		}
		esperaCodigo(t, resp, 201)
	}
	l := decodifica[[]Volumen](t, pide(t, srv, tokenAlice, "GET", "/v1/volumes", nil))
	t.Fatalf("tenant with MaxVolumenes=1 owns %d volumes after clone_from at sandbox creation", len(l))
}
