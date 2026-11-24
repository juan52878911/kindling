package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func instalarConstructor(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestConstructorExterno(t *testing.T) {
	if !construirImagenes {
		t.Skip("macOS no construye imágenes: POST /images contesta 501 (ver TestBuildImageSegunPlataforma)")
	}
	s, h := testServer(t)
	bdir := t.TempDir()
	t.Setenv("KLING_BUILDERS_DIR", bdir)
	t.Setenv("KLING_BUILDERS_INSECURE", "1")
	os.MkdirAll(filepath.Join(s.root, "images"), 0o755)

	// Un constructor que lee su petición y deja la imagen donde toca.
	instalarConstructor(t, bdir, "toy", `
set -e
grep -q '"answer": 42' "$1/request.json"
echo "building $KLING_IMAGE_NAME on ${BASE_IMAGE:-none}"
echo rootfs > "$KLING_ROOT/images/$KLING_IMAGE_NAME.ext4"
`)
	body := `{"name":"juguete","base":"min","builder":"toy","spec":{"answer":42,"secret":"s3"}}`
	rr := call(t, h, "POST", "/images", body)
	if rr.Code != 200 {
		t.Fatalf("construir con constructor externo: %d %s", rr.Code, rr.Body)
	}
	var res api.BuildImageResult
	json.Unmarshal(rr.Body.Bytes(), &res)
	if !strings.Contains(res.Output, "building juguete on min") {
		t.Fatalf("la salida del constructor tiene que volver: %q", res.Output)
	}
	st, err := os.Stat(s.recipePath("juguete"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("la receta debe existir con 0600 (el spec puede llevar secretos): %v %v", st, err)
	}
	var rec api.ImageRecipe
	b, _ := os.ReadFile(s.recipePath("juguete"))
	json.Unmarshal(b, &rec)
	var spec struct{ Answer int }
	json.Unmarshal(rec.Spec, &spec)
	if rec.Builder != "toy" || rec.Base != "min" || spec.Answer != 42 {
		t.Fatalf("receta: %s", b)
	}
	// El directorio de trabajo se limpia.
	if left, _ := filepath.Glob(filepath.Join(s.root, "build", "juguete.*")); len(left) > 0 {
		t.Fatalf("quedó el directorio de trabajo: %v", left)
	}

	instalarConstructor(t, bdir, "roto", "echo 'no puedo'; exit 3\n")
	if rr := call(t, h, "POST", "/images", `{"name":"x","builder":"roto"}`); rr.Code != 500 || !strings.Contains(rr.Body.String(), "no puedo") {
		t.Fatalf("un constructor que falla: %d %s", rr.Code, rr.Body)
	}
	instalarConstructor(t, bdir, "vago", "exit 0\n")
	if rr := call(t, h, "POST", "/images", `{"name":"x","builder":"vago"}`); rr.Code != 500 || !strings.Contains(rr.Body.String(), "left no image") {
		t.Fatalf("un constructor que no deja imagen: %d %s", rr.Code, rr.Body)
	}
	if rr := call(t, h, "POST", "/images", `{"name":"x","builder":"no-existe"}`); rr.Code != 412 {
		t.Fatalf("constructor no instalado: %d", rr.Code)
	}
	if rr := call(t, h, "POST", "/images", `{"name":"x","builder":"../toy"}`); rr.Code != 412 {
		t.Fatalf("un nombre de constructor con ruta: %d", rr.Code)
	}
	if rr := call(t, h, "POST", "/images", `{"name":"../x","builder":"toy"}`); rr.Code != 400 {
		t.Fatalf("un nombre de imagen con ruta: %d", rr.Code)
	}
}

// Sin el modo inseguro de pruebas, un constructor que no es de root no se
// ejecuta: el daemon lo correría como root.
func TestConstructorDebeSerDeRoot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("como root los ficheros del test son de root")
	}
	bdir := t.TempDir()
	t.Setenv("KLING_BUILDERS_DIR", bdir)
	instalarConstructor(t, bdir, "ajeno", "exit 0\n")
	if _, err := builderPath("ajeno"); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("un constructor que no es de root debe rechazarse: %v", err)
	}
}

// El constructor puede dejar recipe.json (api.BuildRecipeHints) y el daemon
// lo lleva a la receta. "android" es de los que corren sin root: también en
// el daemon de macOS, que al resto le contesta 501.
func TestConstructorPistasDeReceta(t *testing.T) {
	s, h := testServer(t)
	bdir := t.TempDir()
	t.Setenv("KLING_BUILDERS_DIR", bdir)
	t.Setenv("KLING_BUILDERS_INSECURE", "1")
	os.MkdirAll(filepath.Join(s.root, "images"), 0o755)
	instalarConstructor(t, bdir, "android", `
set -e
echo capa > "$KLING_ROOT/images/$KLING_IMAGE_NAME.layer.ext4"
echo base > "$KLING_ROOT/images/$KLING_IMAGE_NAME-base.ext4"
echo '{"base": "tel-base", "cpu_pct_per_vcpu": 100, "guest_ipv6_stack": true, "built": {"verity_root_hash": "abc"}}' > "$1/recipe.json"
`)
	rr := call(t, h, "POST", "/images", `{"name":"tel","builder":"android","spec":{}}`)
	if rr.Code != 200 {
		t.Fatalf("android: %d %s", rr.Code, rr.Body)
	}
	var rec api.ImageRecipe
	b, _ := os.ReadFile(s.recipePath("tel"))
	json.Unmarshal(b, &rec)
	if rec.Base != "tel-base" || rec.CPUPctPerVCPU != 100 || !rec.GuestIPv6Stack || !strings.Contains(string(rec.Built), `"abc"`) {
		t.Fatalf("receta: %s", b)
	}

	// Pistas malas: la construcción falla en vez de dejar una receta rara.
	instalarConstructor(t, bdir, "android", `
echo capa > "$KLING_ROOT/images/$KLING_IMAGE_NAME.layer.ext4"
echo '{"base": "../x"}' > "$1/recipe.json"
`)
	if rr := call(t, h, "POST", "/images", `{"name":"tel","builder":"android"}`); rr.Code != 500 || !strings.Contains(rr.Body.String(), "invalid base") {
		t.Fatalf("base inválida en recipe.json: %d %s", rr.Code, rr.Body)
	}
	if !construirImagenes {
		instalarConstructor(t, bdir, "toy", "exit 0\n")
		if rr := call(t, h, "POST", "/images", `{"name":"x","builder":"toy"}`); rr.Code != 501 {
			t.Fatalf("macOS con un constructor de root: %d", rr.Code)
		}
	}
}

// Reconstruir la imagen de un modelo (llm) o de un Chispa mientras una réplica
// la usa no puede reescribir la capa debajo de ella: el constructor deja la
// salida en KLING_OUT_DIR y el daemon la adopta con las imágenes quietas, con
// la misma comprobación que un import -replace. El constructor falso hace lo
// que hacía el script: escribir donde le digan, o en images/ si nadie se lo dice.
func TestRebuildLLMConReplicasVivas409(t *testing.T) {
	if !construirImagenes {
		t.Skip("macOS no construye imágenes")
	}
	for _, b := range []string{"llm", "chispa"} {
		t.Run(b, func(t *testing.T) {
			s, h := testServer(t)
			bdir := t.TempDir()
			t.Setenv("KLING_BUILDERS_DIR", bdir)
			t.Setenv("KLING_BUILDERS_INSECURE", "1")
			os.MkdirAll(filepath.Join(s.root, "images"), 0o755)
			instalarConstructor(t, bdir, b, `
set -e
out="${KLING_OUT_DIR:-$KLING_ROOT/images}"
mkdir -p "$out"
echo "$CONTENIDO" > "$out/$KLING_IMAGE_NAME.layer.ext4"
echo "$CONTENIDO" >> "$KLING_ROOT/corridas"
`)
			capa := filepath.Join(s.root, "images", "modelo.layer.ext4")
			corridas := filepath.Join(s.root, "corridas")
			t.Setenv("CONTENIDO", "v1")
			if rr := call(t, h, "POST", "/images", `{"name":"modelo","base":"min","builder":"`+b+`","spec":{"a":1}}`); rr.Code != 200 {
				t.Fatalf("primera construcción: %d %s", rr.Code, rr.Body)
			}
			if got, _ := os.ReadFile(capa); string(got) != "v1\n" {
				t.Fatalf("la capa no llegó a images/: %q", got)
			}
			s.mgr.SembrarParaPruebas(&api.Machine{ID: "aaaaaaaaaaaa", Name: "replica-1", State: api.StateRunning, Image: "modelo"})
			t.Setenv("CONTENIDO", "v2")
			rr := call(t, h, "POST", "/images", `{"name":"modelo","base":"min","builder":"`+b+`","spec":{"a":2}}`)
			if rr.Code != 409 || !strings.Contains(rr.Body.String(), "replica-1") {
				t.Fatalf("reconstruir con una réplica viva = %d %s; quería 409", rr.Code, rr.Body)
			}
			if got, _ := os.ReadFile(capa); string(got) != "v1\n" {
				t.Fatalf("la capa en uso cambió: %q", got)
			}
			// Se sabe antes de construir: no se gastan minutos en bajar un
			// modelo que luego no se puede adoptar.
			if got, _ := os.ReadFile(corridas); string(got) != "v1\n" {
				t.Fatalf("el constructor corrió aunque la imagen estaba en uso: %q", got)
			}
			// Sin réplicas, reconstruir sustituye la capa.
			s.mgr.SembrarParaPruebas(&api.Machine{ID: "aaaaaaaaaaaa", Name: "replica-1", State: api.StateStopped, Image: "modelo"})
			if rr := call(t, h, "POST", "/images", `{"name":"modelo","base":"min","builder":"`+b+`","spec":{"a":3}}`); rr.Code != 200 {
				t.Fatalf("reconstruir sin réplicas: %d %s", rr.Code, rr.Body)
			}
			if got, _ := os.ReadFile(capa); string(got) != "v2\n" {
				t.Fatalf("la capa no se sustituyó: %q", got)
			}
		})
	}
}
