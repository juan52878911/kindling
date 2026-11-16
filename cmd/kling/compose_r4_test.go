package main

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// F3: el servicio b-web del proyecto a y el web del proyecto a-b se llaman
// los dos a-b-web. El up de a no borra la máquina de a-b como huérfana, y se
// niega a seguir (reemplazaría su imagen); down -v no borra un volumen que
// monta una máquina de otro proyecto.
func TestProyectosQueColisionan(t *testing.T) {
	src := "services:\n  b-web:\n    image: nginx\n    volumes: [\"b-data:/d\"]\nvolumes:\n  b-data:\n"
	tr, err := traducirCompose([]byte(src), "a", true, sinEntorno)
	if err != nil {
		t.Fatal(err)
	}
	ajena := &api.Machine{ID: "x1", Name: "a-b-web", State: api.StateFailed,
		Labels: map[string]string{api.LabelGraph: "g2", labelComposeProject: "a-b"}}
	var mu sync.Mutex
	var borradas []string
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /machines/{ref}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		borradas = append(borradas, r.PathValue("ref"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	c := fakeDaemon(t, mux)
	quitarHuerfanasDe(context.Background(), c, tr, []*api.Machine{ajena})
	if len(borradas) != 0 {
		t.Fatalf("borró la de otro proyecto: %v", borradas)
	}
	if err := colisionDe(tr, []*api.Machine{ajena}, false); err == nil || !strings.Contains(err.Error(), "a-b-web") {
		t.Fatalf("up: %v", err)
	}
	conVol := &api.Machine{ID: "x2", Name: "a-b-otra", Labels: map[string]string{labelComposeProject: "a-b"},
		Volumes: []api.VolumeAttachment{{Name: "a-b-data", Mount: "/d"}}}
	if err := colisionDe(tr, []*api.Machine{conVol}, true); err == nil || !strings.Contains(err.Error(), "volume a-b-data") {
		t.Fatalf("down -v: %v", err)
	}
	// Las suyas y las que no son de compose, no.
	mia := &api.Machine{Name: "a-b-web", Labels: map[string]string{labelComposeProject: "a"}}
	suelta := &api.Machine{Name: "a-b-web"}
	if err := colisionDe(tr, []*api.Machine{mia, suelta}, false); err != nil {
		t.Fatalf("las suyas: %v", err)
	}
}

// S12: la interpolación va por escalar, después de parsear. Un valor con
// " #", ": " o un salto de línea llega entero a su variable y no añade
// claves; ni en un valor sin comillas.
func TestInterpolarTrasParsear(t *testing.T) {
	for _, valor := range []string{"s3cr #et", "a: b", "uno\nOTRA: dos", "x # y: z", "fin #"} {
		src := "services:\n  db:\n    image: postgres:17\n    environment:\n      DB_PASS: ${P}\n      USER: \"${P}\"\n"
		lookup := func(k string) (string, bool) { return valor, k == "P" }
		tr, err := traducirCompose([]byte(src), "dev", false, lookup)
		if err != nil {
			t.Fatalf("%q: %v", valor, err)
		}
		env := tr.vivos[0].env
		if len(env) != 2 || env[0] != [2]string{"DB_PASS", valor} || env[1] != [2]string{"USER", valor} {
			t.Errorf("%q: env = %q", valor, env)
		}
	}
}

// S12: un image: que no es una referencia de imagen (uno que empieza por
// "-" iría como bandera a kling image import) es un error de la traducción.
func TestImagenNoEsReferencia(t *testing.T) {
	for _, img := range []string{"-archive=/x", "--replace", "${IMG}"} {
		src := "services:\n  app:\n    image: \"" + img + "\"\n"
		lookup := func(k string) (string, bool) { return "-archive=/x", k == "IMG" }
		if _, err := traducirCompose([]byte(src), "dev", false, lookup); err == nil || !strings.Contains(err.Error(), "image") {
			t.Errorf("%q: err = %v", img, err)
		}
	}
	src := "services:\n  app:\n    image: ghcr.io/a/b:1.2@sha256:" + strings.Repeat("a", 64) + "\n"
	if _, err := traducirCompose([]byte(src), "dev", false, sinEntorno); err != nil {
		t.Fatalf("una referencia buena: %v", err)
	}
}
