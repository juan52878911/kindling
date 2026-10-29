package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Con el directorio del compose, un bind mount de un directorio es una carpeta
// viva del nodo y los ports: publicados quedan en Publica para compose up.
func TestComposeEnBase(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "nginx.conf"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	c := `services:
  app:
    image: node:22-alpine
    ports: ["3000:3000", "127.0.0.1:9229:9229"]
    volumes: ["./src:/app", "./src:/ro:ro", "./nginx.conf:/etc/nginx.conf"]
`
	tr, err := traducirComposeEn([]byte(c), "dev", false, sinEntorno, base)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(base, "src")
	for _, want := range []string{
		"shares: [{source: " + yq(src) + ", mount: /app, mode: rw}, {source: " + yq(src) + ", mount: /ro, mode: ro}]",
	} {
		if !strings.Contains(tr.Grafo, want) {
			t.Errorf("graph lacks %q:\n%s", want, tr.Grafo)
		}
	}
	if !strings.Contains(strings.Join(tr.Avisos, "\n"), "nginx.conf:/etc/nginx.conf: only directories can be shared") {
		t.Errorf("no warning for a file bind mount: %v", tr.Avisos)
	}
	if got := tr.Publica["app"]; !reflect.DeepEqual(got, []api.PortPublish{{Host: 3000, Guest: 3000}, {Host: 9229, Guest: 9229}}) {
		t.Errorf("Publica = %v", got)
	}
	// Sin directorio (graph from-compose), como antes: se avisa y se salta.
	tr, err = traducirCompose([]byte(c), "dev", false, sinEntorno)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tr.Grafo, "shares:") {
		t.Errorf("from-compose without a base made shares:\n%s", tr.Grafo)
	}
}

func TestShellWords(t *testing.T) {
	l := "kling image import node:22 -name a-b -e " + comillaSh("X=a b'c") + " -- node /app/x.js"
	got, err := shellWords(l)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"kling", "image", "import", "node:22", "-name", "a-b", "-e", "X=a b'c", "--", "node", "/app/x.js"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("shellWords = %q", got)
	}
	if _, err := shellWords("a 'b"); err == nil {
		t.Error("unbalanced quote accepted")
	}
}
