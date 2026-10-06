package main

// `kling run -image <referencia de Docker>`: la imagen se importa sola.
//
// Con `kling image import` el flujo era de dos pasos, y el nombre de la
// imagen importada, otra cosa que recordar. Como en `docker run`, una
// referencia (`redis:7-alpine`, `ghcr.io/o/r:tag`, `postgres@sha256:...`) se
// importa la primera vez y después se reutiliza por su nombre de siempre
// (imageNameFor). El entorno de -e va DENTRO de la imagen (ver imagenes.md),
// así que dos entornos distintos son dos imágenes: el nombre lleva un sufijo
// con el hash del entorno, y ni las claves ni los valores aparecen en él.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/juan52878911/kindling/internal/oci"
	"github.com/juan52878911/kindling/pkg/api"
)

// esRefDocker dice si lo que se pasó a -image es una referencia de Docker y
// no el nombre de una imagen de kindling: un nombre de kindling nunca lleva
// ':', '/' ni '@'.
func esRefDocker(image string) bool {
	if !strings.ContainsAny(image, ":/@") {
		return false
	}
	_, err := oci.ParseImageRef(image)
	return err == nil
}

// nombreParaRef es el nombre de la imagen de kindling para una referencia y
// un entorno: el de imageNameFor y, con entorno, "-" y seis hexadecimales del
// hash de sus líneas ordenadas.
func nombreParaRef(r oci.ImageRef, env []string) string {
	name := imageNameFor(r)
	if len(env) == 0 {
		return name
	}
	lineas := append([]string(nil), env...)
	sort.Strings(lineas)
	sum := sha256.Sum256([]byte(strings.Join(lineas, "\n")))
	return name + "-" + hex.EncodeToString(sum[:3])
}

// asegurarImagenDocker devuelve el nombre de la imagen de kindling de la
// referencia ref con el entorno env, importándola si el daemon no la tiene.
// Lo que dice mientras importa va a stderr: la salida normal del run puede
// ser JSON.
func asegurarImagenDocker(ctx context.Context, c *api.Client, ref string, env []string) (string, error) {
	r, err := oci.ParseImageRef(ref)
	if err != nil {
		return "", err
	}
	for _, kv := range env {
		if !reBuildEnv.MatchString(kv) {
			k, _, _ := strings.Cut(kv, "=")
			return "", fmt.Errorf("invalid environment entry %q: use KEY=value, one line", k)
		}
	}
	name := nombreParaRef(r, env)
	imgs, err := c.Images(ctx)
	if err != nil {
		return "", err
	}
	for _, im := range imgs {
		if im.Name == name {
			return name, nil
		}
	}
	sb, _ := json.Marshal(OCISpec{Ref: ref, Env: env})
	fmt.Fprintf(os.Stderr, "importing %s as %s (the first time downloads it)...\n", r, name)
	res, err := c.BuildImage(ctx, api.BuildImageRequest{Name: name, Builder: "oci", Spec: sb})
	if err != nil {
		if res != nil && res.Output != "" {
			fmt.Fprint(os.Stderr, res.Output)
		}
		return "", fmt.Errorf("importing %s: %w", ref, err)
	}
	return res.Name, nil
}
