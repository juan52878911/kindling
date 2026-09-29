package main

// Los motores de kling db. Postgres es el de siempre (sin etiqueta); MySQL/
// MariaDB (#64, mysql.go), Redis y SQLite (#65, aquí) se distinguen por la
// etiqueta kling.db.engine que ponen sus scripts de plantilla y que heredan
// las copias.
//
// Redis (scripts/db-golden-redis.sh): el mismo modelo que MySQL. Una copia es
// una microVM con redis-server caliente; el usuario de la aplicación es un
// usuario ACL cuya clave estrena cada copia al nacer (up, fork, reset) y solo
// vive en el host: al invitado va su SHA-256 (ACL SETUSER ... #<hash>), por
// stdin, nunca la clave. La administración va por el socket local con el
// usuario default, cuya clave tampoco sale del invitado: se genera dentro, en
// /etc/kling-db/redis-admin (0600, root), y cada copia la estrena también al
// prepararse, para que dos copias del mismo dorado no compartan ninguna.
//
// SQLite (scripts/db-golden-sqlite.sh): no hay servidor ni red ni contraseña.
// La copia es una microVM con el fichero /var/lib/kling-db/<base>.sqlite y el
// cliente sqlite3; se entra con kling exec o kling shell (kling db connect
// -sqlite), que ya exigen ser el dueño de la máquina en el daemon. Por eso una
// copia SQLite no tiene fichero de contraseña en el host y checkReady no lo
// pide.
//
// Qué NO hacen Redis ni SQLite en esta versión (se rechaza con un error claro):
// attach/detach (no hay proxy de credenciales para estos protocolos), role,
// rehearse, snapshot/undo, tenant-check, ask/ask-web, diff, audit, branch,
// env y clone. rotate vale para Redis (tiene clave), no para SQLite.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/plugin"
)

const (
	// labelEngine es el motor de la copia: "mysql", "redis", "sqlite" o, sin
	// etiqueta, Postgres (las copias anteriores a #64 no la llevan).
	labelEngine = "kling.db.engine"

	enginePostgres = "postgres"
	engineMySQL    = "mysql"
	engineRedis    = "redis"
	engineSQLite   = "sqlite"

	redisPort = 6379
)

// engineOf es el motor de unas etiquetas. Un valor desconocido cuenta como
// Postgres, como antes de #64 (goldenInfo sí lo rechaza al crear copias).
func engineOf(labels map[string]string) string {
	switch e := labels[labelEngine]; e {
	case engineMySQL, engineRedis, engineSQLite:
		return e
	}
	return enginePostgres
}

// knownEngine dice si e es un motor que kling db sabe manejar.
func knownEngine(e string) bool {
	switch e {
	case enginePostgres, engineMySQL, engineRedis, engineSQLite:
		return true
	}
	return false
}

// enginePort es el puerto del servidor de un motor; 0 si no tiene (SQLite).
func enginePort(engine string) int {
	switch engine {
	case engineMySQL:
		return myPort
	case engineRedis:
		return redisPort
	case engineSQLite:
		return 0
	}
	return pgPort
}

// engineDoc es la página que explica qué hace kling db con ese motor.
func engineDoc(engine string) string {
	switch engine {
	case engineMySQL:
		return "docs/mysql.md"
	case engineRedis, engineSQLite:
		return "docs/db-engines.md"
	}
	return "docs/db.md"
}

// hasPassword dice si las copias de ese motor tienen contraseña en el host.
func hasPassword(engine string) bool { return engine != engineSQLite }

// requirePostgres rechaza las operaciones que esta versión solo sabe hacer
// sobre Postgres.
func requirePostgres(mc *api.Machine, cmd string) error {
	return requireEngine(mc, cmd, enginePostgres)
}

// requireEngine rechaza cmd si la copia no es de uno de esos motores.
func requireEngine(mc *api.Machine, cmd string, ok ...string) error {
	e := engineOf(mc.Labels)
	for _, o := range ok {
		if e == o {
			return nil
		}
	}
	return fmt.Errorf("kling db %s supports %s copies only in this version; %s is %s (see %s)",
		cmd, strings.Join(ok, " and "), mc.Name, e, engineDoc(e))
}

// requireGoldenEngine es requireEngine para una plantilla (antes de crear
// nada a partir de ella).
func requireGoldenEngine(s *api.Snapshot, cmd string, ok ...string) error {
	_, _, e, err := goldenInfo(s)
	if err != nil {
		return fmt.Errorf("template %s: %w", s.Name, err)
	}
	for _, o := range ok {
		if e == o {
			return nil
		}
	}
	return fmt.Errorf("kling db %s supports %s templates only in this version; %s is %s (see %s)",
		cmd, strings.Join(ok, " and "), s.Name, e, engineDoc(e))
}

// clientMode es el flag de connect que abre el cliente de cada motor.
func clientMode(engine string) string {
	switch engine {
	case engineMySQL:
		return "mysql"
	case engineRedis:
		return "redis"
	case engineSQLite:
		return "sqlite"
	}
	return "psql"
}

// ── Redis ────────────────────────────────────────────────────────────────────

const (
	redisSock      = "/run/redis/redis.sock"
	redisAdminFile = "/etc/kling-db/redis-admin"
	// redisRotateMarker va en el guion de rotación (los tests lo buscan).
	redisRotateMarker = "kling-db:redis-rotate"
)

// redisAdmin es el cliente del administrador dentro de la copia: el usuario
// default por el socket local, con su clave en REDISCLI_AUTH (entorno, no
// argv) leída del fichero que solo root puede leer. La orden es fija.
const redisAdmin = `c=$(command -v redis-cli || command -v valkey-cli) && REDISCLI_AUTH=$(cat ` + redisAdminFile + `) && export REDISCLI_AUTH && exec "$c" -s ` + redisSock

// redisPing dice si el servidor contesta al administrador.
const redisPing = redisAdmin + " PING"

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// redisHash es lo que Redis guarda de una clave: SHA-256 en hexadecimal.
func redisHash(pw string) string {
	s := sha256.Sum256([]byte(pw))
	return hex.EncodeToString(s[:])
}

// redisReservedUsers no pueden ser el usuario de la aplicación.
var redisReservedUsers = map[string]bool{"default": true}

// waitRedis espera a que el servidor de la copia conteste al administrador.
func (a *app) waitRedis(ctx context.Context, id string) error {
	deadline := time.Now().Add(a.readyWait)
	for {
		out, err := a.k.Run(ctx, nil, "exec", "-timeout", "10s", id, "--", "sh", "-c", redisPing)
		if err == nil && strings.TrimSpace(string(out)) == "PONG" {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("redis in %s does not answer after %s", shortID(id), a.readyWait)
		}
		a.sleep(time.Second)
	}
}

// redisRotateCommon abre el guion: el cliente del administrador con la clave
// vigente.
const redisRotateCommon = `C=$(command -v redis-cli || command -v valkey-cli)
S=` + redisSock + `
F=` + redisAdminFile + `
REDISCLI_AUTH=$(cat "$F")
export REDISCLI_AUTH
`

// redisRotateAdmin estrena la clave del administrador dentro del invitado. El
// fichero nuevo se escribe antes de cambiarla y solo sustituye al viejo cuando
// la nueva ya vale: nunca queda una clave de administrador perdida.
const redisRotateAdmin = `N=$(od -An -tx1 -N24 /dev/urandom | tr -d ' \n')
[ ${#N} -eq 48 ] || { echo "no random bytes" >&2; exit 1; }
H=$(printf %s "$N" | sha256sum | cut -d' ' -f1)
printf %s "$N" > "$F.new"
if [ "$(printf 'ACL SETUSER default resetpass #%s\n' "$H" | "$C" -s "$S")" != OK ]; then
  rm -f "$F.new"; echo "the admin password was not rotated" >&2; exit 1
fi
REDISCLI_AUTH=$N
[ "$("$C" -s "$S" PING)" = PONG ] || { echo "the new admin password is not in force" >&2; exit 1; }
mv -f "$F.new" "$F"
`

// redisRotateApp pone el hash $A al usuario $U, lo guarda en el aclfile (por
// si el servidor se reinicia) y enseña el usuario para comprobarlo.
const redisRotateApp = `if [ "$(printf 'ACL SETUSER %s resetpass #%s\nACL SAVE\n' "$U" "$A" | "$C" -s "$S" | tr '\n' ' ')" != "OK OK " ]; then
  echo "the password was not rotated" >&2; exit 1
fi
printf 'ACL GETUSER %s\n' "$U" | "$C" -s "$S"
`

// redisRotateScript es el guion (para sh -s, por stdin) que pone el hash h al
// usuario user y, con admin, estrena antes la clave del administrador.
func redisRotateScript(user, h string, admin bool) string {
	var b strings.Builder
	b.WriteString("# " + redisRotateMarker + "\nset -eu\numask 077\n")
	fmt.Fprintf(&b, "U=%s\nA=%s\n", user, h)
	b.WriteString(redisRotateCommon)
	if admin {
		b.WriteString(redisRotateAdmin)
	}
	b.WriteString(redisRotateApp)
	return b.String()
}

// redisHolds dice si la salida de ACL GETUSER es la de un usuario activo con
// exactamente esa clave.
func redisHolds(out, h string) bool {
	on := false
	var hashes []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "on":
			on = true
		case hex64.MatchString(l):
			hashes = append(hashes, l)
		}
	}
	return on && len(hashes) == 1 && hashes[0] == h
}

// rotateRedis estrena la clave del usuario de la aplicación (y la del
// administrador) en la copia id. Como rotate: la clave se escribe en el host
// ANTES de tocar la base, y al invitado solo va su hash.
func (a *app) rotateRedis(ctx context.Context, id, user string) error {
	pw, err := generatePassword()
	if err != nil {
		return err
	}
	if err := dbstate.WritePassword(id, pw); err != nil {
		return fmt.Errorf("storing the password of %s: %w", shortID(id), err)
	}
	return a.setRedisHash(ctx, id, user, redisHash(pw), true)
}

// setRedisHash pone el hash h al usuario user de la copia id y comprueba que
// es su única clave.
func (a *app) setRedisHash(ctx context.Context, id, user, h string, admin bool) error {
	// user pasó identPattern y h son 64 hexadecimales: nada que escapar.
	if !identPattern.MatchString(user) || redisReservedUsers[user] || !hex64.MatchString(h) {
		return fmt.Errorf("rotating the password of %s: invalid user or hash", shortID(id))
	}
	out, err := a.k.Run(ctx, strings.NewReader(redisRotateScript(user, h, admin)),
		"exec", "-i", "-timeout", "60s", id, "--", "sh", "-s")
	if err != nil {
		return fmt.Errorf("rotating the password of %s failed (output omitted)", shortID(id))
	}
	if !redisHolds(string(out), h) {
		return fmt.Errorf("rotating the password of %s: user %q does not hold the new hash", shortID(id), user)
	}
	return nil
}

// runHostRedis abre el redis-cli (o valkey-cli) del host con la clave en
// REDISCLI_AUTH, nunca en argv. Sin TLS: el tramo es el del host a su propio
// invitado.
func runHostRedis(ctx context.Context, env, args []string) error {
	p, err := exec.LookPath("redis-cli")
	if err != nil {
		if p, err = exec.LookPath("valkey-cli"); err != nil {
			return errors.New("redis-cli is not installed on this host: use kling db connect -dsn with your client")
		}
	}
	c := exec.CommandContext(ctx, p, args...)
	c.Env = append(os.Environ(), env...)
	return runInteractive(c)
}

// ── SQLite ───────────────────────────────────────────────────────────────────

// sqliteDir es donde db-golden-sqlite.sh deja la base dentro del invitado.
const sqliteDir = "/var/lib/kling-db"

// sqlitePath es el fichero de la base db (identPattern: nada que escapar).
func sqlitePath(db string) string { return sqliteDir + "/" + db + ".sqlite" }

// sqliteOpen es la orden (para sh -c) que abre la base en solo lectura, sin
// crearla si no existe (sqlite3 crearía una vacía).
func sqliteOpen(db string) string {
	return `f=` + sqlitePath(db) + ` && [ -f "$f" ] && exec sqlite3 -bail -readonly "$f"`
}

// waitSQLite comprueba que la copia tiene su base y que sqlite3 la abre.
func (a *app) waitSQLite(ctx context.Context, id, db string) error {
	if !identPattern.MatchString(db) {
		return fmt.Errorf("invalid database %q", db)
	}
	deadline := time.Now().Add(a.readyWait)
	for {
		out, err := a.k.Run(ctx, strings.NewReader("PRAGMA schema_version;\n"), "exec", "-i", "-timeout", "60s", id, "--", "sh", "-c", sqliteOpen(db))
		if err == nil && isDigits(strings.TrimSpace(string(out))) {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sqlite in %s cannot open %s", shortID(id), sqlitePath(db))
		}
		a.sleep(time.Second)
	}
}

// connectSQLite es connect para una copia SQLite: sin dirección ni clave;
// -sqlite abre el sqlite3 de la copia con kling shell.
func (a *app) connectSQLite(ctx context.Context, mc *api.Machine, db, mode string) error {
	if !identPattern.MatchString(db) {
		return fmt.Errorf("invalid database %q", db)
	}
	p := sqlitePath(db)
	if mode == "sqlite" {
		return a.runKling(ctx, []string{"shell", mc.ID, "--", "sqlite3", p})
	}
	fmt.Fprintf(a.stdout, "%s  ready  (machine %s)\n", mc.Name, shortID(mc.ID))
	fmt.Fprintf(a.stdout, "  engine    sqlite (no network server, no password)\n  file      %s   (inside the copy)\n", p)
	fmt.Fprintf(a.stdout, "  kling db connect %s -sqlite   ·   kling exec -i %s -- sqlite3 -bail %s < script.sql\n", mc.Name, mc.Name, p)
	return nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ── clientes interactivos ────────────────────────────────────────────────────

// runInteractive ejecuta c con la terminal del usuario. Ctrl-C es del hijo
// (cancela la consulta), no nuestro; su código de salida es el nuestro.
func runInteractive(c *exec.Cmd) error {
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	signal.Ignore(os.Interrupt)
	err := c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return &plugin.ExitError{Code: ee.ExitCode()}
	}
	return err
}
