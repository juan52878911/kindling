package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
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
