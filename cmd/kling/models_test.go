package main

import (
	"strings"
	"testing"
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
