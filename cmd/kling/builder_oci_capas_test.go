package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/internal/oci/ocitest"
	"github.com/juan52878911/kindling/pkg/api"
)

// Una imagen cuya cadena de capas empieza por la de otra ya importada se
// guarda como capa sobre ella, con solo la diferencia; si la base ya no está,
// entera.
func TestBuildOCIReutilizaBase(t *testing.T) {
	e := newOCITest(t)
	base := ocitest.TarGz(alpineLike())
	extra := ocitest.TarGz([]ocitest.File{
		{Name: "opt/", Dir: true, Mode: 0o755}, {Name: "opt/nuevo", Body: "capa dos\n"},
		{Name: "etc/.wh.passwd", Body: ""},
	})
	_, idxA := e.reg.ImageConfig("amd64", map[string]any{"Cmd": []string{"a"}}, base)
	_, idxB := e.reg.ImageConfig("amd64", map[string]any{"Cmd": []string{"b"}}, base, extra)
	images := filepath.Join(e.root, "images")
	// La receta que escribe el daemon a partir de lo que deja el constructor.
	guardar := func(name string, h api.BuildRecipeHints) {
		rec, _ := json.Marshal(api.ImageRecipe{Name: name, Builder: "oci", Base: h.Base, Built: h.Built})
		if err := os.WriteFile(filepath.Join(images, name+".recipe.json"), rec, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	hA, log, err := e.build("a", OCISpec{Ref: e.reg.Host() + "/x/a@" + idxA, Arch: "amd64"})
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if hA.Base != "" {
		t.Fatalf("la primera no tiene base: %q", hA.Base)
	}
	guardar("a", hA)

	hB, log, err := e.build("b", OCISpec{Ref: e.reg.Host() + "/x/b@" + idxB, Arch: "amd64"})
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if hB.Base != "a" {
		t.Fatalf("base = %q, quiero a\n%s", hB.Base, log)
	}
	if _, err := os.Stat(filepath.Join(images, "b.ext4")); err == nil {
		t.Fatal("b.ext4 entero junto a la capa")
	}
	tree, read := readImage(t, filepath.Join(images, "b.layer.ext4"))
	if got := read("/upper/opt/nuevo"); got != "capa dos\n" {
		t.Fatalf("opt/nuevo = %q", got)
	}
	if w, _ := tree.Resolve("/upper/etc/passwd"); w == nil || w.Mode&ext4.ModeType != ext4.ModeChar {
		t.Fatalf("falta la marca de borrado de etc/passwd: %+v", w)
	}
	for _, p := range []string{"/upper/bin/busybox", "/upper/usr/local/bin/kling-guest", "/upper/sbin/overlay-init", "/upper/var/old"} {
		if n, _ := tree.Resolve(p); n != nil {
			t.Fatalf("%s está en la capa y es de la base", p)
		}
	}
	if !strings.Contains(read("/upper/etc/kindling/IMAGE.txt"), "ref=") {
		t.Fatal("falta IMAGE.txt de la capa")
	}
	guardar("b", hB)

	// Sin la base, la misma imagen sale entera.
	if err := os.Remove(filepath.Join(images, "a.ext4")); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(images, "b.layer.ext4"))
	hC, log, err := e.build("c", OCISpec{Ref: e.reg.Host() + "/x/b@" + idxB, Arch: "amd64"})
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if hC.Base != "" {
		t.Fatalf("base borrada reutilizada: %q", hC.Base)
	}
	if _, err := os.Stat(filepath.Join(images, "c.ext4")); err != nil {
		t.Fatal(err)
	}
}
