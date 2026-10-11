package doctor

// Revisión de una copia ClickHouse de kling db (etiqueta
// kling.db.engine=clickhouse): por el cliente del administrador dentro del
// invitado (el usuario default, solo desde el loopback). Reglas CHnnn. Lo que
// devuelve el servidor es dato no fiable: se escapa antes de imprimirlo.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/juan52878911/kindling/ext/db/internal/klingc"
	"github.com/juan52878911/kindling/pkg/api"
)

// chAdmin es el cliente del administrador dentro de la copia (el mismo que usa
// kling db); las consultas van por stdin.
const chAdmin = `exec clickhouse-client --host 127.0.0.1 --multiquery`

var reCHSHA = regexp.MustCompile(`IDENTIFIED WITH sha256_hash BY '([0-9A-Fa-f]{64})'`)

func chQuery(ctx context.Context, k klingc.Kling, id, sql string) (string, error) {
	out, err := k.Run(ctx, strings.NewReader(sql+"\n"), "exec", "-i", id, "--", "sh", "-c", chAdmin)
	if err != nil {
		return "", fmt.Errorf("clickhouse-client in %s: %w", id, err)
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

func clickhouseCopy(ctx context.Context, k klingc.Kling, mc *api.Machine, state string, r *report) error {
	golden := mc.Labels[LabelGolden]
	appUser, err := myAppUser(state, golden, mc.Labels)
	if err != nil {
		return err
	}
	names, err := chQuery(ctx, k, mc.ID, "SELECT name FROM system.users ORDER BY name;")
	if err != nil {
		return fmt.Errorf("doctor: cannot query ClickHouse in %s: %w", safe(mc.Name, 64), err)
	}
	var otros []string
	tieneApp := false
	for _, n := range strings.Split(names, "\n") {
		n = strings.TrimSpace(n)
		switch n {
		case "":
		case appUser:
			tieneApp = true
		case "default":
		default:
			otros = append(otros, q(n))
		}
	}
	if len(otros) > 0 {
		sort.Strings(otros)
		r.add("CH003", Warn, "remove users the application does not need (DROP USER)",
			"users besides default and the application's: %s", strings.Join(otros, ", "))
	}
	// default (el administrador, sin clave) solo desde el propio invitado.
	if def, err := chQuery(ctx, k, mc.ID, "SHOW CREATE USER default FORMAT TSVRaw;"); err != nil {
		checkFailed(r, "CH001", "the default user", err)
	} else if !chSoloLocal(def) {
		r.add("CH001", Critical, "restrict default to 127.0.0.1 and ::1 (users.d of the golden, scripts/db-golden-clickhouse.sh)",
			"user default (no password) may log in from the network: %s", q(def))
	}

	var app *redisUser
	if tieneApp {
		sc, err := chQuery(ctx, k, mc.ID, "SHOW CREATE USER "+appUser+" SETTINGS format_display_secrets_in_show_and_select = 1 FORMAT TSVRaw;")
		if err != nil {
			checkFailed(r, "CH052", "the application user", err)
			return nil
		}
		app = &redisUser{name: appUser, on: true}
		if m := reCHSHA.FindStringSubmatch(sc); m != nil {
			app.hashes = []string{strings.ToLower(m[1])}
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
		r.add("CH053", High, "finish preparing the copy (password rotation) or destroy it; kling db connect refuses it until then",
			"%s is %s, not %q", LabelState, q(mc.Labels[LabelState]), StateReady)
	}
	hashPasswordChecks("CH", "scripts/db-golden-clickhouse.sh", app, mc.ID, state, golden, appUser, ready, r)
	return nil
}

// reCHHostIP son las IP de la cláusula HOST IP '...' de un SHOW CREATE USER.
var reCHHostIP = regexp.MustCompile(`IP '([^']*)'`)

// chSoloLocal dice si el SHOW CREATE USER de un usuario solo le deja entrar
// desde el propio invitado: HOST LOCAL (lo que da users.d con 127.0.0.1 y ::1),
// o HOST IP con solo direcciones del loopback.
func chSoloLocal(def string) bool {
	_, host, ok := strings.Cut(def, " HOST ")
	if !ok {
		return false // sin cláusula HOST: desde cualquier sitio
	}
	if strings.HasPrefix(host, "LOCAL") {
		return true
	}
	if !strings.HasPrefix(host, "IP ") {
		return false // ANY, NAME, REGEXP, LIKE: no se da por local
	}
	if strings.Contains(host, "NAME ") || strings.Contains(host, "REGEXP ") || strings.Contains(host, "LIKE ") {
		return false
	}
	ips := reCHHostIP.FindAllStringSubmatch(host, -1)
	for _, m := range ips {
		if m[1] != "127.0.0.1" && m[1] != "::1" {
			return false
		}
	}
	return len(ips) > 0
}
