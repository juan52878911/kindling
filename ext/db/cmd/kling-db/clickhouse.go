package main

// ClickHouse (#118): copias de una plantilla de ClickHouse (la imagen oficial
// de Docker, scripts/db-golden-clickhouse.sh).
//
// La regla de kling db se cumple igual que en Redis: al invitado solo va el
// SHA-256 de la clave (ALTER USER ... IDENTIFIED WITH sha256_hash BY '<hex>'),
// calculado en el host. El administrador es el usuario default, que solo entra
// desde el loopback del invitado y sin clave: quien llega ahí ya es root
// dentro. La plantilla enciende display_secrets_in_show_and_select para que
// SHOW CREATE USER enseñe el hash guardado: así se comprueba la rotación (y
// doctor la suya) sin que la clave entre nunca.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
)

const chPort = 9000

// chAdmin es el cliente del administrador dentro de la copia; las consultas
// van por stdin. La orden es fija.
const chAdmin = `exec clickhouse-client --host 127.0.0.1 --multiquery`

// chReservedUsers no pueden ser el usuario de la aplicación.
var chReservedUsers = map[string]bool{"default": true}

// reCHHash saca el hash de un SHOW CREATE USER con los secretos a la vista (en
// TSVRaw: sin TSVRaw, el cliente escapa las comillas).
var reCHHash = regexp.MustCompile(`IDENTIFIED WITH sha256_hash BY '([0-9A-Fa-f]{64})'`)

// waitClickHouse espera a que el servidor de la copia conteste al administrador.
func (a *app) waitClickHouse(ctx context.Context, id string) error {
	deadline := time.Now().Add(a.readyWait)
	for {
		out, err := a.k.Run(ctx, strings.NewReader("SELECT 1;\n"), "exec", "-i", "-timeout", "10s", id, "--", "sh", "-c", chAdmin)
		if err == nil && strings.TrimSpace(string(out)) == "1" {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("clickhouse in %s does not answer after %s", shortID(id), a.readyWait)
		}
		a.sleep(time.Second)
	}
}

// rotateClickHouse estrena la clave del usuario de la aplicación en la copia
// id. Como rotate: la clave se escribe en el host ANTES de tocar la base, y al
// invitado solo va su hash.
func (a *app) rotateClickHouse(ctx context.Context, id, user string) error {
	pw, err := generatePassword()
	if err != nil {
		return err
	}
	if err := dbstate.WritePassword(id, pw); err != nil {
		return fmt.Errorf("storing the password of %s: %w", shortID(id), err)
	}
	return a.setClickHouseHash(ctx, id, user, redisHash(pw))
}

// setClickHouseHash pone el hash h al usuario user de la copia id y comprueba
// con SHOW CREATE USER que es el que guarda.
func (a *app) setClickHouseHash(ctx context.Context, id, user, h string) error {
	// user pasó identPattern y h son 64 hexadecimales: nada que escapar.
	if !identPattern.MatchString(user) || chReservedUsers[user] || !hex64.MatchString(h) {
		return fmt.Errorf("rotating the password of %s: invalid user or hash", shortID(id))
	}
	sql := fmt.Sprintf("ALTER USER %[1]s IDENTIFIED WITH sha256_hash BY '%[2]s';\n"+
		"SHOW CREATE USER %[1]s SETTINGS format_display_secrets_in_show_and_select = 1 FORMAT TSVRaw;\n", user, h)
	out, err := a.k.Run(ctx, strings.NewReader(sql), "exec", "-i", "-timeout", "60s", id, "--", "sh", "-c", chAdmin)
	if err != nil {
		return fmt.Errorf("rotating the password of %s failed (output omitted)", shortID(id))
	}
	m := reCHHash.FindStringSubmatch(string(out))
	if m == nil || !strings.EqualFold(m[1], h) {
		return fmt.Errorf("rotating the password of %s: user %q does not hold the new hash", shortID(id), user)
	}
	return nil
}
