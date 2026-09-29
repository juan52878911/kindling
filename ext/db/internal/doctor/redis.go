package doctor

// Revisión de una copia Redis de kling db (etiqueta kling.db.engine=redis):
// lo básico, por el cliente del administrador dentro del invitado (el usuario
// default, socket local, clave en un fichero que solo lee root). Reglas
// RDnnn. Lo que devuelve el servidor (nombres de usuario, reglas ACL) es dato
// no fiable: se escapa antes de imprimirlo y no se ejecuta.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/ext/db/internal/klingc"
	"github.com/juan52878911/kindling/pkg/api"
)

// redisAdmin es el cliente del administrador dentro de la copia (el mismo que
// usa kling db): los comandos van por stdin; la orden es fija.
const redisAdmin = `c=$(command -v redis-cli || command -v valkey-cli) && REDISCLI_AUTH=$(cat /etc/kling-db/redis-admin) && export REDISCLI_AUTH && exec "$c" -s /run/redis/redis.sock`

// RedisAdminClient es redisAdmin para el test de kling db que comprueba que
// los dos lados usan la misma orden.
const RedisAdminClient = redisAdmin

var reHex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// redisCmd manda un comando (fijo, o con un nombre ya validado) al cliente
// del administrador y devuelve su salida.
func redisCmd(ctx context.Context, k klingc.Kling, id, cmd string) (string, error) {
	out, err := k.Run(ctx, strings.NewReader(cmd+"\n"), "exec", "-i", id, "--", "sh", "-c", redisAdmin)
	if err != nil {
		return "", fmt.Errorf("redis-cli in %s: %w", id, err)
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

// redisUser es una línea de ACL LIST: "user <nombre> on|off [nopass] #<hash> ...".
type redisUser struct {
	name   string
	on     bool
	nopass bool
	hashes []string
}

func parseACLList(out string) []redisUser {
	var us []redisUser
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(strings.TrimSpace(l))
		if len(f) < 2 || f[0] != "user" {
			continue
		}
		u := redisUser{name: f[1]}
		for _, x := range f[2:] {
			switch {
			case x == "on":
				u.on = true
			case x == "nopass":
				u.nopass = true
			case strings.HasPrefix(x, "#"):
				u.hashes = append(u.hashes, strings.TrimPrefix(x, "#"))
			}
		}
		us = append(us, u)
	}
	return us
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// redisCopy revisa una copia Redis. mc ya se leyó y está corriendo.
func redisCopy(ctx context.Context, k klingc.Kling, mc *api.Machine, state string, r *report) error {
	golden := mc.Labels[LabelGolden]
	appUser, err := myAppUser(state, golden, mc.Labels)
	if err != nil {
		return err
	}
	out, err := redisCmd(ctx, k, mc.ID, "ACL LIST")
	if err != nil {
		return fmt.Errorf("doctor: cannot query Redis in %s: %w", safe(mc.Name, 64), err)
	}
	users := parseACLList(out)
	if len(users) == 0 {
		return fmt.Errorf("doctor: unexpected ACL LIST output from %s", safe(mc.Name, 64))
	}
	var app *redisUser
	var otros []string
	for i := range users {
		u := &users[i]
		if u.name == appUser {
			app = u
		}
		if !u.on {
			continue
		}
		if u.nopass {
			r.add("RD001", Critical, "ACL SETUSER "+safe(u.name, 64)+" resetpass followed by a password hash (or off)",
				"user %s logs in with any password (nopass)", q(u.name))
		}
		if u.name != "default" && u.name != appUser {
			otros = append(otros, q(u.name))
		}
	}
	if len(otros) > 0 {
		sort.Strings(otros)
		r.add("RD003", Warn, "remove users the application does not need (ACL DELUSER)",
			"enabled users besides default and the application's: %s", strings.Join(otros, ", "))
	}
	if app != nil && app.on {
		// El usuario ya pasó reRole: nada que escapar en el comando.
		if dr, err := redisCmd(ctx, k, mc.ID, "ACL DRYRUN "+appUser+" CONFIG GET maxmemory"); err != nil {
			checkFailed(r, "RD002", "the application user's permissions", err)
		} else if strings.TrimSpace(dr) == "OK" {
			r.add("RD002", High, "ACL SETUSER "+appUser+" -@admin (the golden script gives it +@all -@admin)",
				"user %s can run admin commands (CONFIG, ACL, SHUTDOWN...)", q(appUser))
		}
	}

	if golden == "" {
		r.add("DB059", Info, "", "not a kling db copy (no %s label): copy checks skipped", LabelGolden)
		return nil
	}
	if !reGolden.MatchString(golden) {
		r.add("DB059", High, "recreate the copy from a golden with a plain name",
			"label %s has an unexpected value %s: copy checks skipped", LabelGolden, q(golden))
		return nil
	}
	ready := mc.Labels[LabelState] == StateReady
	if !ready {
		r.add("RD053", High, "finish preparing the copy (password rotation) or destroy it; kling db connect refuses it until then",
			"%s is %s, not %q", LabelState, q(mc.Labels[LabelState]), StateReady)
	}
	redisPasswordChecks(app, mc.ID, state, golden, appUser, ready, r)

	if cl, err := redisCmd(ctx, k, mc.ID, "CLIENT LIST"); err != nil {
		checkFailed(r, "RD051", "client connections", err)
	} else if n := strings.Count(strings.TrimSpace(cl), "\n"); n > 0 {
		// Una línea por conexión; la del propio doctor no cuenta.
		r.add("RD051", Info, "", "%d client connection(s) open (the agent's, or inherited by a fork of a live copy)", n)
	}
	return nil
}

func redisPasswordChecks(app *redisUser, id, state, golden, appUser string, ready bool, r *report) {
	if app == nil || !app.on {
		r.add("RD052", High, "recreate the copy from a golden built by scripts/db-golden-redis.sh",
			"application user %s does not exist or is off", q(appUser))
		return
	}
	if len(app.hashes) != 1 || !reHex64.MatchString(app.hashes[0]) {
		r.add("RD052", High, "rotate the password with kling db rotate (it leaves exactly one)",
			"user %s has %d password(s); kling db gives it exactly one", q(appUser), len(app.hashes))
		return
	}
	gdir := filepath.Join(state, golden)
	if gp, err := readSmall(filepath.Join(gdir, "password")); err == nil {
		if sha256Hex(strings.TrimRight(string(gp), "\r\n")) == app.hashes[0] {
			r.add("RD052", Critical, "destroy this copy; kling db must rotate the password before marking a copy ready",
				"the password of %s is still the golden's: whoever knows %s's password can log in to this copy", q(appUser), q(golden))
		}
	} else {
		r.add("RD052", Warn, fmt.Sprintf("keep the golden's password in %s so the rotation can be checked", filepath.Join(gdir, "password")),
			"cannot verify that the password of %s was rotated away from the golden's", q(appUser))
	}

	p := filepath.Join(state, dbstate.CopiesDir, id, "password")
	fi, err := os.Lstat(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
		sev := Info
		if ready {
			sev = High
		}
		r.add("RD054", sev, "destroy the copy and create it again: the rotated password is only known to the host that rotated it",
			"no host password file for this copy (%s): kling db connect refuses it", p)
		return
	case err != nil:
		checkFailed(r, "RD054", "the host password file", err)
		return
	case !fi.Mode().IsRegular():
		r.add("RD054", High, "replace it with a regular file (0600)", "host password file %s is not a regular file", p)
		return
	case fi.Mode().Perm()&0o077 != 0:
		r.add("RD054", High, fmt.Sprintf("chmod 600 %s", p),
			"host password file %s is readable by other users (mode %04o)", p, fi.Mode().Perm())
	}
	cp, err := readSmall(p)
	if err != nil {
		checkFailed(r, "RD054", "the host password file", err)
		return
	}
	if sha256Hex(strings.TrimRight(string(cp), "\r\n")) != app.hashes[0] {
		r.add("RD054", High, "rotate the password again (kling db rotate) or recreate the copy",
			"host password file %s does not match the user's hash: kling db connect will fail", p)
	}
}
