package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/juan52878911/kindling/internal/oci"
)

// Un nombre de kindling nunca es una referencia; una referencia lleva
// etiqueta, registro o digest.
func TestEsRefDocker(t *testing.T) {
	for in, want := range map[string]bool{
		"hs-full2":                       false,
		"default":                        false,
		"redis:7-alpine":                 true,
		"postgres:17-alpine":             true,
		"ghcr.io/vectorize-io/hindsight": true,
		"docker.io/library/nginx":        true,
		"nginx@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef": true,
	} {
		if got := esRefDocker(in); got != want {
			t.Errorf("%q: %v, quería %v", in, got, want)
		}
	}
}

// El nombre sale de la referencia y lleva un sufijo que cambia con la
// referencia entera y con la clave, y ya no con el entorno (va en la
// máquina): una imagen por referencia. El sufijo es el mismo que daba la
// versión anterior sin -e, para que lo ya importado conserve su nombre.
func TestNombreParaRef(t *testing.T) {
	ref := func(s string) oci.ImageRef {
		r, err := oci.ParseImageRef(s)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	clave := []byte("clave de prueba de 32 bytes.....")
	r := ref("postgres:17-alpine")
	sin := nombreParaRef(r, clave)
	if !strings.HasPrefix(sin, "postgres-17-alpine-") || len(sin) != len("postgres-17-alpine-")+8 {
		t.Errorf("nombre: %q", sin)
	}
	// La fórmula de antes con el entorno vacío: HMAC(ref + "\x00" + "").
	mac := hmac.New(sha256.New, clave)
	mac.Write([]byte(r.String() + "\x00"))
	if want := "postgres-17-alpine-" + hex.EncodeToString(mac.Sum(nil)[:4]); sin != want {
		t.Errorf("cambió el nombre de una imagen ya importada sin -e: %q, antes %q", sin, want)
	}
	// Otra referencia con el mismo nombre corto, o la misma fijada por
	// digest, no reutiliza la imagen.
	d := "@sha256:" + strings.Repeat("ab", 32)
	for _, otra := range []string{"ghcr.io/evil/postgres:17-alpine", "postgres:17-alpine" + d} {
		if nombreParaRef(ref(otra), clave) == sin {
			t.Errorf("%s se llama igual que postgres:17-alpine", otra)
		}
	}
	if nombreParaRef(r, []byte("otra clave")) == sin {
		t.Error("el sufijo no depende de la clave")
	}
}

// run -image avisa si la imagen que reutiliza es de antes de las políticas
// de reinicio (su servicio se relanza siempre) y dice cómo rehacerla; con
// política, o de otro constructor, no dice nada.
func TestAvisoImportAntiguo(t *testing.T) {
	ctx := context.Background()
	c := fakeDaemon(t, fakeImagesMux("oci", `{"ref":"redis:7"}`))
	aviso := avisoImportAntiguo(ctx, c, "redis-7", "docker.io/library/redis:7")
	for _, w := range []string{"before restart policies", "kling image import docker.io/library/redis:7 -name redis-7 -replace"} {
		if !strings.Contains(aviso, w) {
			t.Errorf("aviso %q no dice %q", aviso, w)
		}
	}
	for _, mux := range []*http.ServeMux{
		fakeImagesMux("oci", `{"ref":"redis:7","restart":"on-failure"}`),
		fakeImagesMux("debian", `{"packages":["redis"]}`),
	} {
		if a := avisoImportAntiguo(ctx, fakeDaemon(t, mux), "redis-7", "redis:7"); a != "" {
			t.Errorf("avisó sin motivo: %q", a)
		}
	}
	// Sin receta (404), el run sigue sin avisar.
	if a := avisoImportAntiguo(ctx, fakeDaemon(t, http.NewServeMux()), "redis-7", "redis:7"); a != "" {
		t.Errorf("sin receta: %q", a)
	}
}

// asegurarImagenDocker, al reutilizar la imagen que ya tiene para la
// referencia, avisa si es de antes de las políticas de reinicio, y no la
// rehace por su cuenta (nada de POST /images/build).
func TestAsegurarImagenDockerAvisaAlReutilizar(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("HOME", cfg)
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("AppData", cfg)
	r, err := oci.ParseImageRef("redis:7")
	if err != nil {
		t.Fatal(err)
	}
	name := nombreParaRef(r, claveRunImage())
	m := http.NewServeMux()
	m.HandleFunc("GET /images", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"` + name + `","has_recipe":true}]`))
	})
	m.HandleFunc("GET /images/{name}/recipe", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"` + name + `","builder":"oci","spec":{"ref":"redis:7"}}`))
	})
	var construyo atomic.Bool
	m.HandleFunc("POST /images/build", func(w http.ResponseWriter, _ *http.Request) {
		construyo.Store(true)
		http.Error(w, "no", http.StatusInternalServerError)
	})
	c := fakeDaemon(t, m)

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = pw
	got, err := asegurarImagenDocker(context.Background(), c, "redis:7")
	os.Stderr = stderr
	pw.Close()
	salida, _ := io.ReadAll(pr)
	if err != nil || got != name {
		t.Fatalf("asegurarImagenDocker: %q, %v", got, err)
	}
	if construyo.Load() {
		t.Error("rehízo la imagen por su cuenta")
	}
	if !strings.Contains(string(salida), "before restart policies") || !strings.Contains(string(salida), "-name "+name+" -replace") {
		t.Errorf("no avisó de la imagen antigua: %q", salida)
	}
}
