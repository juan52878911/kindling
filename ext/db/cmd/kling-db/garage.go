package main

// Almacén de objetos S3 (#115): copias de una plantilla de Garage (la imagen
// oficial dxflrs/garage, scripts/db-golden-garage.sh).
//
// SigV4 es un HMAC: el servidor necesita el secreto, no un hash. La regla de
// kling db se cumple en lo que sí se puede: cada copia estrena su PROPIO par
// acceso/secreto aleatorio, calculado en el host (el secreto es la
// contraseña de la copia; el identificador sale de él, dbstate.S3AccessKey),
// y la clave del dorado se borra de la copia antes de entregarla. El secreto
// sí entra en el invitado (en argv de `garage key import`, que solo se ve
// dentro de la copia): no hay forma de dar a Garage solo un verificador.
//
// La imagen es scratch (sin sh): todo va por argv directo a /garage, con la
// configuración en /etc/garage.toml.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
)

const (
	engineGarage = "garage"
	garagePort   = 3900
)

// garageCLI es el prefijo de toda orden de administración dentro de la copia.
var garageCLI = []string{"/garage", "-c", "/etc/garage.toml"}

// reGarageKey saca los identificadores de acceso de un `garage key list`.
var reGarageKey = regexp.MustCompile(`\bGK[0-9a-f]{24}\b`)

// reBucket: lo que Garage admite como nombre (3-63, minúsculas, dígitos, '-').
var reBucket = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

// garageExec lanza /garage <args> dentro de la copia id.
func (a *app) garageExec(ctx context.Context, id string, args ...string) ([]byte, error) {
	full := append([]string{"exec", "-timeout", "60s", id, "--"}, garageCLI...)
	return a.k.Run(ctx, nil, append(full, args...)...)
}

// waitGarage espera a que el servidor de la copia conteste a su CLI.
func (a *app) waitGarage(ctx context.Context, id string) error {
	deadline := time.Now().Add(a.readyWait)
	for {
		out, err := a.garageExec(ctx, id, "key", "list")
		if err == nil && strings.Contains(string(out), "keys") {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("garage in %s does not answer after %s", shortID(id), a.readyWait)
		}
		a.sleep(time.Second)
	}
}

// rotateGarage estrena el par de claves de la aplicación en la copia id. Como
// rotate: el secreto se escribe en el host ANTES de tocar la copia.
func (a *app) rotateGarage(ctx context.Context, id, user, bucket string) error {
	pw, err := generatePassword()
	if err != nil {
		return err
	}
	if err := dbstate.WritePassword(id, pw); err != nil {
		return fmt.Errorf("storing the password of %s: %w", shortID(id), err)
	}
	return a.setGarageKey(ctx, id, user, bucket, pw)
}

// setGarageKey deja en la copia id UNA sola clave, la de secreto pw, con
// acceso de lectura, escritura y dueño al bucket; borra las demás (la del
// dorado incluida) y lo comprueba.
func (a *app) setGarageKey(ctx context.Context, id, user, bucket, pw string) error {
	ak := dbstate.S3AccessKey(pw)
	// user pasó identPattern y pw son 64 hexadecimales: nada que escapar (y
	// no hay shell de por medio).
	if !identPattern.MatchString(user) || !hex64.MatchString(pw) || !reBucket.MatchString(bucket) {
		return fmt.Errorf("rotating the key of %s: invalid user, secret or bucket", shortID(id))
	}
	fail := func(what string) error {
		// Sin el detalle: el error de la orden cita el secreto.
		return fmt.Errorf("rotating the key of %s failed at %s (output omitted)", shortID(id), what)
	}
	old, err := a.garageExec(ctx, id, "key", "list")
	if err != nil {
		return fail("key list")
	}
	if _, err := a.garageExec(ctx, id, "key", "import", "--yes", "-n", user, ak, pw); err != nil {
		return fail("key import")
	}
	if _, err := a.garageExec(ctx, id, "bucket", "allow", "--read", "--write", "--owner", bucket, "--key", ak); err != nil {
		return fail("bucket allow")
	}
	for _, k := range reGarageKey.FindAllString(string(old), -1) {
		if k == ak {
			continue
		}
		if _, err := a.garageExec(ctx, id, "key", "delete", "--yes", k); err != nil {
			return fail("key delete")
		}
	}
	out, err := a.garageExec(ctx, id, "key", "list")
	if err != nil {
		return fail("key list")
	}
	if ks := reGarageKey.FindAllString(string(out), -1); len(ks) != 1 || ks[0] != ak {
		return fmt.Errorf("rotating the key of %s: the copy does not hold exactly the new key", shortID(id))
	}
	return nil
}
