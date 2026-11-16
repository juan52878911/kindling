package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// El camino secreto del muro no va en el argv del navegador (lo ve `ps`): va
// en un fichero 0600 que redirige a él y que se borra después.
func TestAbrirMuroNoPoneElSecretoEnElArgv(t *testing.T) {
	var argv []string
	antes, antesPlazo := lanzarNavegador, abrirMuroPlazo
	lanzarNavegador = func(nombre string, args ...string) error {
		argv = append([]string{nombre}, args...)
		return nil
	}
	abrirMuroPlazo = 50 * time.Millisecond
	t.Cleanup(func() { lanzarNavegador, abrirMuroPlazo = antes, antesPlazo })

	u := "http://127.0.0.1:8765/c0ffee5ecre7/"
	if err := abrirMuro(u); err != nil {
		t.Fatal(err)
	}
	if len(argv) != 2 {
		t.Fatalf("argv = %q", argv)
	}
	for _, a := range argv {
		if strings.Contains(a, "c0ffee5ecre7") {
			t.Fatalf("el secreto va en el argv: %q", argv)
		}
	}
	b, err := os.ReadFile(argv[1])
	if err != nil || !strings.Contains(string(b), u) {
		t.Fatalf("la página no redirige al muro: %q, %v", b, err)
	}
	if fi, _ := os.Stat(argv[1]); fi.Mode().Perm() != 0o600 {
		t.Errorf("la página es %v; quería 0600", fi.Mode().Perm())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(argv[1]); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("la página de redirección no se borró")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
