package main

// `kling run -image <referencia de Docker>`: la imagen se importa sola.
//
// Con `kling image import` el flujo era de dos pasos, y el nombre de la
// imagen importada, otra cosa que recordar. Como en `docker run`, una
// referencia (`redis:7-alpine`, `ghcr.io/o/r:tag`, `postgres@sha256:...`) se
// importa la primera vez y después se reutiliza. El nombre es el de
// imageNameFor más un sufijo que sale de la referencia ENTERA (registro,
// repositorio, etiqueta y digest): ni `ghcr.io/x/redis:7` se hace pasar por
// `redis:7`, ni fijar un digest reutiliza otra cosa.
//
// Una imagen por referencia: el entorno de -e ya no va dentro de la imagen
// sino en la máquina (pkg/api/machine_env.go), así que dos contraseñas son
// dos máquinas sobre la misma imagen. El sufijo sigue siendo un HMAC con una
// clave local (claveRunImage) y con el mismo formato que cuando llevaba el
// entorno, vacío: así las imágenes ya importadas sin -e conservan su nombre.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// nombreParaRef es el nombre de la imagen de kindling para una referencia:
// el de imageNameFor, "-" y ocho hexadecimales del HMAC de la referencia
// normalizada (y un entorno vacío, por compatibilidad: ver arriba).
func nombreParaRef(r oci.ImageRef, clave []byte) string {
	mac := hmac.New(sha256.New, clave)
	mac.Write([]byte(r.String() + "\x00"))
	base := imageNameFor(r)
	if len(base) > 55 { // los nombres de imagen tienen 64 como mucho
		base = strings.TrimRight(base[:55], "-_")
	}
	return base + "-" + hex.EncodeToString(mac.Sum(nil)[:4])
}

// claveRunImage es la clave local del HMAC de nombreParaRef: 32 bytes en
// <config>/kling/run-image.key (0600), creada la primera vez. Si no se puede
// leer ni crear, nil: el sufijo es entonces un hash sin clave. Ya no protege
// nada (el nombre solo depende de la referencia); se conserva para que los
// nombres de lo ya importado no cambien.
func claveRunImage() []byte {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil
	}
	ruta := filepath.Join(dir, "kling", "run-image.key")
	if b, err := os.ReadFile(ruta); err == nil && len(b) == 32 {
		return b
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(ruta), 0o700); err != nil {
		return nil
	}
	f, err := os.OpenFile(ruta, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		// Otro proceso la creó a la vez: la suya.
		if b2, err := os.ReadFile(ruta); err == nil && len(b2) == 32 {
			return b2
		}
		return nil
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return nil
	}
	return b
}

// asegurarImagenDocker devuelve el nombre de la imagen de kindling de la
// referencia ref, importándola si el daemon no la tiene. Lo que dice
// mientras importa va a stderr: la salida normal del run puede ser JSON.
func asegurarImagenDocker(ctx context.Context, c *api.Client, ref string) (string, error) {
	r, err := oci.ParseImageRef(ref)
	if err != nil {
		return "", err
	}
	name := nombreParaRef(r, claveRunImage())
	imgs, err := c.Images(ctx)
	if err != nil {
		return "", err
	}
	for _, im := range imgs {
		if im.Name == name {
			return name, nil
		}
	}
	sb, _ := json.Marshal(OCISpec{Ref: ref, Restart: api.RestartOnFailure})
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
