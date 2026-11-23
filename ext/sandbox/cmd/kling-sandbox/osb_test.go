package main

import (
	"strings"
	"testing"
)

func TestSbxOSBRegistrado(t *testing.T) {
	if err := sbxOSB([]string{"-tls-cert", "x.pem"}); err == nil || !strings.Contains(err.Error(), "go together") {
		t.Fatalf("esperaba el error de -tls-cert sin -tls-key: %v", err)
	}
	if err := sbxOSB([]string{"-image-map", "sin-igual"}); err == nil || !strings.Contains(err.Error(), "uri=template") {
		t.Fatalf("esperaba el error de -image-map: %v", err)
	}
	m := mapaImagenes{}
	if m.Set("python:3.12=py") != nil || m["python:3.12"] != "py" {
		t.Fatalf("mapa: %v", m)
	}
	if !strings.Contains(ayuda, "sbx osb") {
		t.Fatal("la ayuda no menciona sbx osb")
	}
	// El mensaje de subcomando desconocido lista todos, también shell y osb (C28).
	err := cmdSbx([]string{"nada"})
	for _, sub := range []string{"shell", "e2b", "osb"} {
		if err == nil || !strings.Contains(err.Error(), sub) {
			t.Fatalf("el error no lista %s: %v", sub, err)
		}
	}
}
