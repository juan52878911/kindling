package frontal

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/pkg/api"
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

// Los volúmenes nuevos llevan el prefijo largo; los de antes (4 bytes) siguen
// viéndose, listándose y montándose por su nombre.
func TestVolumenPrefijoLargoYLecturaDelCorto(t *testing.T) {
	b := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"b": b}, Tenant{Nombre: "alice", Token: tokenAlice})
	alice := &Tenant{Nombre: "alice"}
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/volumes", CrearVolumenPeticion{Name: "nuevo", SizeMiB: 64}), 201)
	if !b.tieneVolumen(prefijoVolumen(alice) + "nuevo") {
		t.Fatalf("el volumen nuevo no lleva el prefijo largo %q", prefijoVolumen(alice))
	}
	if len(prefijoVolumen(alice)) != 18 {
		t.Fatalf("prefijo %q", prefijoVolumen(alice))
	}
	b.sembrarVolumen(&api.Volume{Name: prefijoCorto(alice) + "viejo", SizeBytes: 64 << 20, Labels: etiquetasVolumen(alice)})
	b.sembrarVolumen(&api.Volume{Name: prefijoCorto(alice) + "clonviejo", SizeBytes: 64 << 20}) // un clon: sin etiqueta
	l := decodifica[[]Volumen](t, pide(t, srv, tokenAlice, "GET", "/v1/volumes", nil))
	if len(l) != 3 {
		t.Fatalf("lista %+v, quería nuevo, viejo y clonviejo", l)
	}
	esperaCodigo(t, pide(t, srv, tokenAlice, "GET", "/v1/volumes/viejo", nil), 200)
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/volumes", CrearVolumenPeticion{Name: "viejo"}), 409)
	resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base",
		Volumes: []VolumenPeticion{{Name: "viejo", Mount: "/data"}}})
	esperaCodigo(t, resp, 201)
}

// Un clon (sin etiqueta) con prefijo corto que comparten dos tenants no es de
// ninguno; con el prefijo largo, o etiquetado, sí.
func TestVolumenSinEtiquetaConPrefijoCortoCompartido(t *testing.T) {
	vistos := map[string]string{}
	var a, c string
	for i := 0; a == ""; i++ {
		n := fmt.Sprintf("t%d", i)
		p := prefijoCorto(&Tenant{Nombre: n})
		if o, ok := vistos[p]; ok {
			a, c = o, n
		}
		vistos[p] = n
	}
	b := nuevoFalso(t)
	_, s := montar(t, map[string]*falso{"b": b},
		Tenant{Nombre: a, Token: tokenAlice}, Tenant{Nombre: c, Token: tokenBob})
	ta, tc := &Tenant{Nombre: a}, &Tenant{Nombre: c}
	clon := &api.Volume{Name: prefijoCorto(ta) + "x"}
	if s.esVolumenDe(clon, ta) || s.esVolumenDe(clon, tc) {
		t.Errorf("un clon con prefijo corto compartido se atribuyó a alguien")
	}
	if !s.esVolumenDe(&api.Volume{Name: prefijoCorto(ta) + "x", Labels: etiquetasVolumen(ta)}, ta) {
		t.Errorf("el etiquetado debería ser de su tenant")
	}
	if !s.esVolumenDe(&api.Volume{Name: prefijoVolumen(ta) + "x"}, ta) || s.esVolumenDe(&api.Volume{Name: prefijoVolumen(ta) + "x"}, tc) {
		t.Errorf("el prefijo largo debería bastar y distinguir")
	}
}

// Arrancar con un tenant sin MaxSandboxes ni MaxMemTotalMiB avisa.
func TestAvisoDeCuotasAbiertas(t *testing.T) {
	var buf bytes.Buffer
	_, err := Nuevo(Opciones{Hosts: hosts.Nuevo(map[string]string{}), Log: log.New(&buf, "", 0),
		Tenants: []Tenant{{Nombre: "abierto", Token: tokenAlice}, {Nombre: "tope", Token: tokenBob, MaxSandboxes: 3}}})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"abierto"`) || strings.Contains(out, `"tope"`) {
		t.Fatalf("aviso: %q", out)
	}
}
