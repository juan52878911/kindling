package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/oci"
)

// Los argumentos que puso quien importa no se repiten en la salida.
func TestRunsLine(t *testing.T) {
	argv := []string{"postgres", "-c", "password=secreto"}
	if got := runsLine(argv, false); got != "postgres -c password=secreto" {
		t.Fatalf("de la imagen: %q", got)
	}
	got := runsLine(argv, true)
	if strings.Contains(got, "secreto") || !strings.HasPrefix(got, "postgres") || !strings.Contains(got, "2 arguments") {
		t.Fatalf("de la línea de órdenes: %q", got)
	}
}

// -restart se comprueba antes de hablar con el daemon (no hay ninguno).
func TestImagesImportRestartFlag(t *testing.T) {
	err := imagesImport([]string{"-H", "/nonexistent/kling.sock", "-restart", "sometimes", "redis:7"})
	if err == nil || !strings.Contains(err.Error(), "-restart must be") {
		t.Fatalf("err = %v", err)
	}
}

// Una imagen que solo expone UDP se queda sin sonda: la salida lo dice.
func TestReadyLine(t *testing.T) {
	udp := oci.ImageConfig{ExposedPorts: map[string]struct{}{"53/udp": {}, "5353/udp": {}}}
	script, what := ociReadyProbe(udp)
	if script != "" || what != "" {
		t.Fatalf("UDP con sonda: %q %q", script, what)
	}
	got := readyLine(what, sortedKeys(udp.ExposedPorts))
	if !strings.HasPrefix(got, "none") || !strings.Contains(got, "53/udp 5353/udp") || !strings.Contains(got, "-wait-ready") {
		t.Fatalf("solo UDP: %q", got)
	}
	if got := readyLine("tcp 5432", []string{"5432/tcp"}); got != "tcp 5432" {
		t.Fatalf("con sonda: %q", got)
	}
	if got := readyLine("", nil); got != "" {
		t.Fatalf("sin puertos: %q", got)
	}
}

// fakeImagesMux es un daemon con dos imágenes: "redis-7", hecha por builder
// con spec, y "vieja", sin receta.
func fakeImagesMux(builder, spec string) *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /images", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"redis-7","has_recipe":true},{"name":"vieja"}]`))
	})
	m.HandleFunc("GET /images/{name}/recipe", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"redis-7","builder":"` + builder + `","spec":` + spec + `}`))
	})
	return m
}

// imageNameFor da el mismo nombre a redis:7 y a ghcr.io/x/redis:7: importar
// el segundo no pisa en silencio el primero.
func TestExistingImport(t *testing.T) {
	ctx := context.Background()
	c := fakeDaemon(t, fakeImagesMux("oci", `{"ref":"redis:7","env":["A=secreto"]}`))
	mismo := OCISpec{Ref: "docker.io/library/redis:7", Env: []string{"A=secreto"}}
	if ok, err := existingImport(ctx, c, "redis-7", mismo); err != nil || !ok {
		t.Fatalf("la misma importación: %v %v", ok, err)
	}
	if ok, err := existingImport(ctx, c, "otra", mismo); err != nil || ok {
		t.Fatalf("sin imagen con ese nombre: %v %v", ok, err)
	}
	_, err := existingImport(ctx, c, "redis-7", OCISpec{Ref: "ghcr.io/x/redis:7", Env: []string{"A=secreto"}})
	if err == nil || !strings.Contains(err.Error(), "docker.io/library/redis:7") || !strings.Contains(err.Error(), "-replace") {
		t.Fatalf("otra referencia: %v", err)
	}
	_, err = existingImport(ctx, c, "redis-7", OCISpec{Ref: "redis:7", Env: []string{"A=otro"}})
	if err == nil || !strings.Contains(err.Error(), "other options") || strings.Contains(err.Error(), "secreto") || strings.Contains(err.Error(), "otro") {
		t.Fatalf("misma referencia, otras opciones: %v", err)
	}
	_, err = existingImport(ctx, c, "redis-7", OCISpec{Ref: "redis:7@sha256:" + strings.Repeat("a", 64), Env: []string{"A=secreto"}})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("otro digest: %v", err)
	}
	if _, err := existingImport(ctx, c, "vieja", mismo); err == nil || !strings.Contains(err.Error(), "wasn't imported") {
		t.Fatalf("sin receta: %v", err)
	}
	c = fakeDaemon(t, fakeImagesMux("debian", `{"packages":["redis"]}`))
	if _, err := existingImport(ctx, c, "redis-7", mismo); err == nil || !strings.Contains(err.Error(), "wasn't imported") {
		t.Fatalf("de otro constructor: %v", err)
	}
}
