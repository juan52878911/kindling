package main

import (
	"context"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/von"
	"github.com/juan52878911/kindling/pkg/api"
)

// -prefix no tiene sentido en un codificador (kind embed): no genera texto a
// partir de un prompt largo y fijo, así que no hay nada que dejar evaluado en
// el dorado. El rechazo pasa antes de tocar el daemon: ni lee el fichero del
// prefijo ni necesita uno de verdad.
func TestModelsAddPrefixConCodificadorRechaza(t *testing.T) {
	err := modelsAdd([]string{"-model", "multilingual-e5-small", "-prefix", "/no/existe", "mi-codificador"})
	if err == nil || !strings.Contains(err.Error(), "encoder (kind embed)") {
		t.Fatalf("esperaba el rechazo del -prefix en un codificador, got %v", err)
	}
}

// -cache-ram con un codificador es igual de contradictorio, y falla en
// Spec.Resolve antes de llegar al -prefix.
func TestModelsAddCacheRAMConCodificadorRechaza(t *testing.T) {
	err := modelsAdd([]string{"-model", "multilingual-e5-small", "-cache-ram", "64", "mi-codificador"})
	if err == nil || !strings.Contains(err.Error(), "cache-ram") {
		t.Fatalf("esperaba el rechazo del -cache-ram en un codificador, got %v", err)
	}
}

// C9-16: -keep-cache va a la receta y se repite en las instrucciones.
func TestModelFlagsKeepCache(t *testing.T) {
	if f := modelFlags(von.Spec{Model: "smollm2-360m-instruct", KeepCache: true}); !strings.Contains(f, "-keep-cache") {
		t.Fatalf("flags: %s", f)
	}
	if f := modelFlags(von.Spec{Model: "smollm2-360m-instruct"}); strings.Contains(f, "-keep-cache") {
		t.Fatalf("flags: %s", f)
	}
}

// Reconstruir la imagen de un modelo con su dorado delante da 409 en el daemon
// (la imagen está en uso). Con -replace -rebuild el dorado se borra antes,
// salvo que tenga réplicas vivas; sin -replace, se dice qué hacer.
func TestModelsAddRebuildConDorado(t *testing.T) {
	ctx := context.Background()
	d := &daemonModelos{snaps: []*api.Snapshot{{Name: "von-q8", Image: "von-q8"}}}
	if err := prepararReconstruccion(ctx, d.cliente(t), "von-q8", true, true); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(d.llamadas, "|"); got != "rm-snapshot von-q8" {
		t.Fatalf("llamadas: %s", got)
	}
	// Con réplicas vivas no se borra nada: se dice.
	d = &daemonModelos{snaps: []*api.Snapshot{{Name: "von-q8", Image: "von-q8", Instances: 2}}}
	err := prepararReconstruccion(ctx, d.cliente(t), "von-q8", true, true)
	if err == nil || !strings.Contains(err.Error(), "2 machine") || len(d.llamadas) != 0 {
		t.Fatalf("err %v, llamadas %v", err, d.llamadas)
	}
	// -rebuild sin -replace (p. ej. con -build-only) y el dorado vivo: el
	// daemon daría 409; se dice antes de construir.
	d = &daemonModelos{snaps: []*api.Snapshot{{Name: "von-q8", Image: "von-q8"}}}
	err = prepararReconstruccion(ctx, d.cliente(t), "von-q8", false, true)
	if err == nil || !strings.Contains(err.Error(), "-replace") || !strings.Contains(err.Error(), "409") || len(d.llamadas) != 0 {
		t.Fatalf("err %v, llamadas %v", err, d.llamadas)
	}
	// Otro dorado de la misma imagen también la retiene, y ese no es nuestro.
	d = &daemonModelos{snaps: []*api.Snapshot{{Name: "von-q8", Image: "von-q8"}, {Name: "copia", Image: "von-q8"}}}
	err = prepararReconstruccion(ctx, d.cliente(t), "von-q8", true, true)
	if err == nil || !strings.Contains(err.Error(), "copia") || len(d.llamadas) != 0 {
		t.Fatalf("err %v, llamadas %v", err, d.llamadas)
	}
	// Sin -rebuild, o sin dorado, nada que hacer.
	for _, d := range []*daemonModelos{{}, {snaps: []*api.Snapshot{{Name: "von-q8", Image: "von-q8"}}}} {
		rebuild := len(d.snaps) == 0
		if err := prepararReconstruccion(ctx, d.cliente(t), "von-q8", true, rebuild); err != nil || len(d.llamadas) != 0 {
			t.Fatalf("err %v, llamadas %v", err, d.llamadas)
		}
	}
}
