package main

// `kling db golden -from-oci <ref> build ...`: el golden sale de una imagen
// OCI (postgres:16, pgvector/pgvector:pg16, la de la propia app) en vez de la
// pg16 que construye el script. Lo que la imagen ya trae (extensiones
// incluidas) no se compila. Se importa con `kling image import` y el build
// sigue siendo db-golden.sh con -image: initdb, rol, migraciones y guardado
// son los de siempre.

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/juan52878911/kindling/ext/db/internal/klingc"
)

var reNoNombre = regexp.MustCompile(`[^a-z0-9]+`)

// nombreImagenOCI es el nombre de la imagen importada para ese ref. Lleva un
// prefijo propio: la importación no es la plana (el servicio de la imagen va
// apagado) y `kling image import` no pisa una imagen con otra receta.
func nombreImagenOCI(ref string) string {
	n := strings.Trim(reNoNombre.ReplaceAllString(strings.ToLower(ref), "-"), "-")
	n = strings.TrimPrefix(n, "docker-io-")
	if len(n) > 50 {
		n = strings.Trim(n[:50], "-")
	}
	return "dbg-" + n
}

// importarOCI importa el ref con el servicio de la imagen apagado (sleep): el
// golden arranca su propio Postgres, y el de la imagen sin POSTGRES_PASSWORD
// ni se levanta. Lo que ya está importado igual no se rehace.
func importarOCI(ctx context.Context, k klingc.Kling, ref string) (string, error) {
	if strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, " \t\n") {
		return "", usageErr("invalid -from-oci reference %q", ref)
	}
	name := nombreImagenOCI(ref)
	if _, err := k.Run(ctx, nil, "image", "import", ref, "-name", name, "-restart", "no",
		"-entrypoint", "sleep", "-entrypoint", "infinity"); err != nil {
		return "", fmt.Errorf("importing %s: %w", ref, err)
	}
	return name, nil
}

// ociArgs reescribe los argumentos de `golden build` para construir sobre la
// imagen importada.
func ociArgs(rest []string, image string) ([]string, error) {
	if len(rest) == 0 || rest[0] != "build" {
		return nil, usageErr("-from-oci goes with build: kling db golden -from-oci <ref> build [options] <name>")
	}
	for _, a := range rest[1:] {
		f := strings.TrimLeft(a, "-")
		f, _, _ = strings.Cut(f, "=")
		if strings.HasPrefix(a, "-") && (f == "image" || f == "from" || f == "engine") {
			return nil, usageErr("-from-oci replaces -%s: don't give both", f)
		}
	}
	return append([]string{"build", "-image", image, "-loopback"}, rest[1:]...), nil
}
