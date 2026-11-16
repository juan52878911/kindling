package main

import (
	"strings"
	"testing"
)

// doctor avisa de un kernel o un min.ext4 bajados de otra release (kling up
// no los sustituye al actualizar) y calla con los de esta, sin marca o con
// un kling de desarrollo.
func TestCheckImagenesRelease(t *testing.T) {
	ambas := map[string]string{"vmlinux": "v0.20.1", "min.ext4": "v0.20.1"}
	casos := []struct {
		nombre    string
		deRelease map[string]string
		cli       string
		aviso     bool
		dice      string
	}{
		{"de otra release", ambas, "1.0.0", true, "vmlinux from v0.20.1, min.ext4 from v0.20.1"},
		{"solo min es vieja", map[string]string{"vmlinux": "v1.0.0", "min.ext4": "v0.20.1"}, "v1.0.0", true, "min.ext4 from v0.20.1"},
		{"de esta release", map[string]string{"vmlinux": "v1.0.0"}, "v1.0.0", false, ""},
		{"sin marca", nil, "1.0.0", false, ""},
		{"kling de desarrollo", ambas, "dev", false, ""},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			got, ok := checkImagenesRelease(c.deRelease, c.cli, "/x/images")
			if ok != c.aviso {
				t.Fatalf("aviso=%v, quería %v: %+v", ok, c.aviso, got)
			}
			if !ok {
				return
			}
			if got.State != doctorWarn || !strings.Contains(got.Detail, c.dice) {
				t.Fatalf("%+v; quería un warn que diga %q", got, c.dice)
			}
			// El arreglo del kernel solo cuando el viejo es el kernel; min.ext4
			// no se manda reemplazar (es la base de las imágenes por capas).
			if strings.Contains(got.Fix, "mv /x/images/vmlinux") != (c.deRelease["vmlinux"] != "v1.0.0") {
				t.Fatalf("arreglo: %q", got.Fix)
			}
		})
	}
}

func TestDoctorChecksImagenesRelease(t *testing.T) {
	in := doctorInput{cliVersion: "v1.0.0", imagesDir: "/x/images",
		imagesRelease: map[string]string{"vmlinux": "v0.20.1"}}
	for _, c := range doctorChecks(in) {
		if c.Name == "images (release)" {
			return
		}
	}
	t.Fatal("doctor no dice que el kernel es de otra release")
}
