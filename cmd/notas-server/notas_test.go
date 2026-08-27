package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// list solo devuelve FICHEROS. Si colara un directorio, quien intente leerlo
// recibiria un EISDIR en vez de una nota, y el servidor devolveria un error que
// no apunta a nada.
func TestListDevuelveFicherosYNoDirectorios(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "una.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "otra.md"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "subcarpeta"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := list(dir)
	if len(got) != 2 {
		t.Fatalf("devolvió %d entradas, esperaba 2: %v", len(got), got)
	}
	for _, p := range got {
		if strings.HasSuffix(p, "subcarpeta") {
			t.Errorf("coló un directorio: %s", p)
		}
		if fi, err := os.Stat(p); err != nil || fi.IsDir() {
			t.Errorf("%s no es un fichero legible", p)
		}
	}
}

// Un directorio que no existe no puede entrar en pánico: el servidor arranca
// antes de que nadie haya creado nada.
func TestListSobreUnDirectorioQueNoExisteDevuelveVacio(t *testing.T) {
	if got := list(filepath.Join(t.TempDir(), "no-existe")); len(got) != 0 {
		t.Errorf("devolvió %v", got)
	}
}

func TestTextEnvuelveComoEsperaElProtocolo(t *testing.T) {
	m := text("hola")
	c, ok := m["content"].([]any)
	if !ok || len(c) != 1 {
		t.Fatalf("estructura inesperada: %#v", m)
	}
	uno, ok := c[0].(map[string]any)
	if !ok {
		t.Fatalf("el elemento no es un objeto: %#v", c[0])
	}
	if uno["type"] != "text" || uno["text"] != "hola" {
		t.Errorf("contenido = %#v", uno)
	}
}
