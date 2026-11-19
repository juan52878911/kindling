package main

import (
	"strings"
	"testing"
)

func TestSbxE2BRegistrado(t *testing.T) {
	err := sbxE2B([]string{"-tls-cert", "x.pem"})
	if err == nil || !strings.Contains(err.Error(), "go together") {
		t.Fatalf("esperaba el error de -tls-cert sin -tls-key: %v", err)
	}
	if !strings.Contains(ayuda, "sbx e2b") {
		t.Fatal("la ayuda no menciona sbx e2b")
	}
}
