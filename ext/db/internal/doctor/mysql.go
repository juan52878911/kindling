package doctor

// Revisión de una copia MySQL/MariaDB de kling db (etiqueta kling.db.engine=
// mysql): las comprobaciones por el cliente del superusuario root dentro del
// invitado (socket local, unix_socket) y las de copia (estado, rotación de la
// contraseña, reloj). Reglas MYnnn. Como en Postgres, todo lo que viene de la
// base es dato no fiable: se escapa antes de imprimirlo y no se ejecuta.
//
// -url no admite MySQL en esta versión: el doctor tendría que hablar el
// protocolo desde el host (y TLS), y eso queda pendiente (docs/mysql.md).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/ext/db/internal/klingc"
	"github.com/juan52878911/kindling/ext/db/internal/mysqlpw"
	"github.com/juan52878911/kindling/pkg/api"
)

// LabelEngine es el motor de una copia ("mysql", "redis", "sqlite"; sin
// etiqueta, Postgres).
const LabelEngine = "kling.db.engine"

// myClient es el cliente del superusuario dentro de la copia (el mismo que
// usa kling db): root por el socket local, sin cabeceras ni escapes.
const myClient = `c=$(command -v mariadb || command -v mysql) && exec "$c" --protocol=socket -uroot -N -B -r`

// execMyQuerier lanza SQL con myClient dentro de la máquina. La SQL va por
// stdin; la orden es fija.
type execMyQuerier struct {
	k  klingc.Kling
	id string
}

func (e *execMyQuerier) query(ctx context.Context, _, name, sql string) (string, error) {
	in := strings.NewReader("/* doctor:" + name + " */ " + sql + ";\n")
	out, err := e.k.Run(ctx, in, "exec", "-i", e.id, "--", "sh", "-c", myClient)
	if err != nil {
		return "", fmt.Errorf("mysql client in %s: %w", e.id, err)
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

const (
	qMyVersion = `SELECT JSON_QUOTE(VERSION())`

	// Cuentas de MariaDB (10.4+): mysql.global_priv, donde están el bloqueo y
	// si es un rol; la vista mysql.user no los enseña. JSON_VALUE de un true
	// da "1" en MariaDB 11 y "true" en versiones anteriores: valen los dos.
	qMyUsersMariaDB = `SELECT COALESCE(JSON_ARRAYAGG(JSON_OBJECT(
  'user', User, 'host', Host,
  'plugin', COALESCE(JSON_VALUE(Priv, '$.plugin'), ''),
  'auth', COALESCE(JSON_VALUE(Priv, '$.authentication_string'), ''),
  'locked', COALESCE(JSON_VALUE(Priv, '$.account_locked'), 'false') IN ('true', '1'),
  'role', COALESCE(JSON_VALUE(Priv, '$.is_role'), 'false') IN ('true', '1'))), '[]')
  FROM mysql.global_priv`

	// Y de MySQL 8.
	qMyUsersMySQL = `SELECT COALESCE(JSON_ARRAYAGG(JSON_OBJECT(
  'user', User, 'host', Host, 'plugin', plugin, 'auth', COALESCE(authentication_string, ''),
  'locked', account_locked = 'Y', 'role', false)), '[]')
  FROM mysql.user`

	qMyPrivs = `SELECT COALESCE(JSON_ARRAYAGG(JSON_OBJECT('grantee', GRANTEE, 'priv', PRIVILEGE_TYPE, 'grantable', IS_GRANTABLE)), '[]')
  FROM information_schema.USER_PRIVILEGES WHERE PRIVILEGE_TYPE <> 'USAGE' OR IS_GRANTABLE = 'YES'`

	// local_infile va como número: MariaDB mete la variable booleana en el
	// JSON como un OFF sin comillas, que no es JSON.
	qMySettings = `SELECT JSON_OBJECT(
  'local_infile', IF(@@GLOBAL.local_infile, 1, 0),
  'secure_file_priv', @@GLOBAL.secure_file_priv,
  'audit', COALESCE((SELECT PLUGIN_STATUS FROM information_schema.PLUGINS WHERE PLUGIN_NAME = 'SERVER_AUDIT'), ''))`

	// Objetos que corren con los permisos de quien los definió.
	qMyDefiners = `SELECT COALESCE(JSON_ARRAYAGG(JSON_OBJECT('kind', k, 'schema', s, 'name', n, 'definer', d)), '[]') FROM (
  SELECT 'view' AS k, TABLE_SCHEMA AS s, TABLE_NAME AS n, DEFINER AS d FROM information_schema.VIEWS WHERE SECURITY_TYPE = 'DEFINER'
  UNION ALL SELECT LOWER(ROUTINE_TYPE), ROUTINE_SCHEMA, ROUTINE_NAME, DEFINER FROM information_schema.ROUTINES WHERE SECURITY_TYPE = 'DEFINER'
  UNION ALL SELECT 'trigger', TRIGGER_SCHEMA, TRIGGER_NAME, DEFINER FROM information_schema.TRIGGERS
  UNION ALL SELECT 'event', EVENT_SCHEMA, EVENT_NAME, DEFINER FROM information_schema.EVENTS
) t WHERE s NOT IN ('mysql', 'sys', 'information_schema', 'performance_schema')`

	qMyClock   = `SELECT UNIX_TIMESTAMP(NOW(6))`
	qMyClients = `SELECT COUNT(*) FROM information_schema.PROCESSLIST
  WHERE ID <> CONNECTION_ID() AND COMMAND <> 'Daemon' AND USER NOT IN ('system user', 'event_scheduler')`
)

type myUserRow struct {
	User   string `json:"user"`
	Host   string `json:"host"`
	Plugin string `json:"plugin"`
	Auth   string `json:"auth"`
	Locked bool   `json:"locked"`
	Role   bool   `json:"role"`
}

type myPrivRow struct {
	Grantee   string `json:"grantee"`
	Priv      string `json:"priv"`
	Grantable string `json:"grantable"`
}

type mySettingsRow struct {
	LocalInfile    int     `json:"local_infile"`
	SecureFilePriv *string `json:"secure_file_priv"`
	Audit          string  `json:"audit"`
}

type myDefinerRow struct {
	Kind    string `json:"kind"`
	Schema  string `json:"schema"`
	Name    string `json:"name"`
	Definer string `json:"definer"`
}

var reGrantee = regexp.MustCompile(`^'(.*)'@'(.*)'$`)

// hostLocal dice si una cuenta solo entra desde el propio invitado.
func hostLocal(h string) bool {
	switch h {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// privPeligroso: con él, una cuenta lee o escribe ficheros del servidor, ve
// o mata las consultas de otros, crea cuentas, reparte permisos o toca la
// configuración del servidor.
func privPeligroso(p string) bool {
	switch p {
	case "SUPER", "FILE", "PROCESS", "SHUTDOWN", "RELOAD", "CREATE USER", "GRANT OPTION", "SET USER", "SYSTEM_USER", "ALL PRIVILEGES":
		return true
	}
	return strings.Contains(p, "ADMIN")
}

// mysqlCopy revisa una copia MySQL. mc ya se leyó y está corriendo.
func mysqlCopy(ctx context.Context, k klingc.Kling, mc *api.Machine, state string, r *report) error {
	golden := mc.Labels[LabelGolden]
	appUser, err := myAppUser(state, golden, mc.Labels)
	if err != nil {
		return err
	}
	qr := &execMyQuerier{k: k, id: mc.ID}
	var version string
	if err := queryJSON(ctx, qr, "", "version", qMyVersion, &version); err != nil {
		return fmt.Errorf("doctor: cannot query MySQL in %s: %w", safe(mc.Name, 64), err)
	}
	mariadb := strings.Contains(version, "MariaDB")
	qUsers := qMyUsersMySQL
	if mariadb {
		qUsers = qMyUsersMariaDB
	}
	var users []myUserRow
	if err := queryJSON(ctx, qr, "", "users", qUsers, &users); err != nil {
		checkFailed(r, "MY001", "the accounts", err)
	}
	var privs []myPrivRow
	if err := queryJSON(ctx, qr, "", "privileges", qMyPrivs, &privs); err != nil {
		checkFailed(r, "MY001", "the global privileges", err)
	}
	cuentas := myAccountChecks(users, privs, appUser, r)

	var st mySettingsRow
	if err := queryJSON(ctx, qr, "", "settings", qMySettings, &st); err != nil {
		checkFailed(r, "MY010", "server settings", err)
	} else {
		if st.LocalInfile != 0 {
			r.add("MY010", Warn, "set local_infile = 0 in the server configuration",
				"local_infile is ON: LOAD DATA LOCAL lets the server read files of the client that connects")
		}
		switch {
		case st.SecureFilePriv == nil:
		case *st.SecureFilePriv == "":
			r.add("MY011", Warn, "set secure_file_priv to an empty dedicated directory (or NULL)",
				"secure_file_priv is empty: an account with FILE can read and write any file the server can")
		}
		if golden != "" && !strings.EqualFold(st.Audit, "ACTIVE") {
			r.add("MY012", Warn, "rebuild the golden with scripts/db-golden-mysql.sh (server_audit with server_audit_events=CONNECT)",
				"the connection log (server_audit) is not active: kling db audit shows only daemon events")
		}
	}

	var defs []myDefinerRow
	if err := queryJSON(ctx, qr, "", "definers", qMyDefiners, &defs); err != nil {
		checkFailed(r, "MY020", "views, routines, triggers and events", err)
	} else {
		myDefinerChecks(defs, cuentas, r)
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
		r.add("MY053", High, "finish preparing the copy (password rotation) or destroy it; kling db connect refuses it until then",
			"%s is %s, not %q", LabelState, q(mc.Labels[LabelState]), StateReady)
	}
	var app *myUserRow
	for i := range users {
		if users[i].User == appUser && users[i].Host == "%" {
			app = &users[i]
		}
	}
	myPasswordChecks(app, mc.ID, state, golden, appUser, ready, users != nil, r)
	myClockChecks(ctx, k, qr, mc.ID, r)
	var n int
	if err := queryJSON(ctx, qr, "", "clients", qMyClients, &n); err != nil {
		checkFailed(r, "MY051", "client connections", err)
	} else if n > 0 {
		r.add("MY051", Info, "", "%d client connection(s) open (the golden is built with none: these are the agent's, or were inherited by a fork of a live copy)", n)
	}
	return nil
}

// myAppUser es el usuario de la aplicación: la etiqueta kling.db.role de la
// copia, o DBUSER del conn.env del dorado, o "app".
func myAppUser(state, golden string, labels map[string]string) (string, error) {
	user := "app"
	if golden != "" && reGolden.MatchString(golden) {
		if b, err := readSmall(filepath.Join(state, golden, "conn.env")); err == nil {
			sc := bufio.NewScanner(bytes.NewReader(b))
			for sc.Scan() {
				if v, ok := strings.CutPrefix(sc.Text(), "DBUSER="); ok {
					user = strings.TrimSpace(v)
				}
			}
		}
	}
	if v := labels["kling.db.role"]; v != "" {
		user = v
	}
	if !reRole.MatchString(user) {
		return "", fmt.Errorf("doctor: application user %q is not a plain identifier", safe(user, 64))
	}
	return user, nil
}

// myAccountChecks revisa cuentas y privilegios globales. Devuelve el
// conjunto de cuentas con privilegios peligrosos ("user@host"), para MY020.
func myAccountChecks(users []myUserRow, privs []myPrivRow, appUser string, r *report) map[string]bool {
	peligrosas := map[string]bool{}
	type cuenta struct{ user, host string }
	porCuenta := map[cuenta][]string{}
	conGrant := map[cuenta]bool{}
	for _, p := range privs {
		m := reGrantee.FindStringSubmatch(p.Grantee)
		if m == nil {
			continue
		}
		c := cuenta{m[1], m[2]}
		if p.Priv != "USAGE" {
			porCuenta[c] = append(porCuenta[c], p.Priv)
		}
		// IS_GRANTABLE va en cada privilegio: GRANT OPTION se cuenta una vez.
		if p.Grantable == "YES" && !conGrant[c] {
			conGrant[c] = true
			porCuenta[c] = append(porCuenta[c], "GRANT OPTION")
		}
	}
	cs := make([]cuenta, 0, len(porCuenta))
	for c := range porCuenta {
		cs = append(cs, c)
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].user+"@"+cs[i].host < cs[j].user+"@"+cs[j].host })
	for _, c := range cs {
		var malos, otros []string
		for _, p := range porCuenta[c] {
			if privPeligroso(p) {
				malos = append(malos, p)
			} else {
				otros = append(otros, p)
			}
		}
		nombre := q(c.user) + "@" + q(c.host)
		if len(malos) > 0 {
			peligrosas[c.user+"@"+c.host] = true
		}
		switch {
		case len(malos) > 0 && hostLocal(c.host):
			r.add("MY001", Info, "", "%s has server-wide privileges (%s) but only logs in from inside the copy", nombre, safe(strings.Join(malos, ", "), 120))
		case len(malos) > 0:
			r.add("MY001", Critical, "REVOKE them; the application only needs privileges on its own database (GRANT ... ON appdb.*)",
				"%s logs in over the network with server-wide privileges: %s", nombre, safe(strings.Join(malos, ", "), 120))
		case len(otros) > 0 && !hostLocal(c.host):
			r.add("MY002", High, "grant on the application database only (ON appdb.*), not on *.*",
				"%s has privileges on every database (%s)", nombre, safe(strings.Join(otros, ", "), 120))
		}
	}
	for _, u := range users {
		if u.Locked || u.Role {
			continue
		}
		nombre := q(u.User) + "@" + q(u.Host)
		if u.User == "" {
			r.add("MY003", Critical, "DROP USER the anonymous account", "anonymous account %s can log in", nombre)
			continue
		}
		switch u.Plugin {
		case "", "mysql_native_password", "caching_sha2_password", "sha256_password", "mysql_old_password":
			if u.Auth == "" {
				r.add("MY004", Critical, "set a password (or lock the account)", "account %s has no password", nombre)
			}
		}
		if u.User == appUser && u.Host == "%" && u.Plugin == mysqlpw.NativePlugin {
			r.add("MY030", Info, "", "%s uses mysql_native_password (SHA-1, unsalted): fine with the 192-bit random password kling db gives each copy, weak with a human one", nombre)
		}
	}
	return peligrosas
}

// myDefinerChecks avisa de vistas, rutinas, disparadores y eventos que
// corren como una cuenta con privilegios de servidor (root, típicamente,
// porque las migraciones del golden corren como root).
func myDefinerChecks(defs []myDefinerRow, peligrosas map[string]bool, r *report) {
	var malos []string
	for _, d := range defs {
		u, h, _ := strings.Cut(d.Definer, "@")
		if u == "root" || peligrosas[u+"@"+h] {
			malos = append(malos, d.Kind+" "+q(d.Schema)+"."+q(d.Name)+" as "+q(d.Definer))
		}
	}
	if len(malos) == 0 {
		return
	}
	n := len(malos)
	if len(malos) > 5 {
		malos = append(malos[:5], "...")
	}
	r.add("MY020", Warn, "recreate them with DEFINER set to the application user (or SQL SECURITY INVOKER for views and routines)",
		"%d object(s) run with the privileges of a server-wide account: %s", n, strings.Join(malos, "; "))
}

func myPasswordChecks(app *myUserRow, id, state, golden, appUser string, ready, leido bool, r *report) {
	if !leido {
		return // el fallo de la consulta ya está en el informe
	}
	if app == nil {
		r.add("MY052", High, "recreate the copy from a golden built by scripts/db-golden-mysql.sh",
			"application user %s@'%%' does not exist in the copy", q(appUser))
		return
	}
	if app.Plugin != mysqlpw.NativePlugin || !mysqlpw.ValidHash(app.Auth) {
		r.add("MY052", High, "rotate the password with kling db rotate (it stores a mysql_native_password hash)",
			"the password of %s is not a mysql_native_password hash kling db can check (plugin %s)", q(appUser), q(app.Plugin))
		return
	}
	gdir := filepath.Join(state, golden)
	if gp, err := readSmall(filepath.Join(gdir, "password")); err == nil {
		if mysqlpw.NativeHash(strings.TrimRight(string(gp), "\r\n")) == app.Auth {
			r.add("MY052", Critical, "destroy this copy; kling db must rotate the password before marking a copy ready",
				"the password of %s is still the golden's: whoever knows %s's password can log in to this copy", q(appUser), q(golden))
		}
	} else {
		r.add("MY052", Warn, fmt.Sprintf("keep the golden's password in %s so the rotation can be checked", filepath.Join(gdir, "password")),
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
		r.add("MY054", sev, "destroy the copy and create it again: the rotated password is only known to the host that rotated it",
			"no host password file for this copy (%s): kling db connect refuses it", p)
		return
	case err != nil:
		checkFailed(r, "MY054", "the host password file", err)
		return
	case !fi.Mode().IsRegular():
		r.add("MY054", High, "replace it with a regular file (0600)", "host password file %s is not a regular file", p)
		return
	case fi.Mode().Perm()&0o077 != 0:
		r.add("MY054", High, fmt.Sprintf("chmod 600 %s", p),
			"host password file %s is readable by other users (mode %04o)", p, fi.Mode().Perm())
	}
	cp, err := readSmall(p)
	if err != nil {
		checkFailed(r, "MY054", "the host password file", err)
		return
	}
	if mysqlpw.NativeHash(strings.TrimRight(string(cp), "\r\n")) != app.Auth {
		r.add("MY054", High, "rotate the password again (kling db rotate) or recreate the copy",
			"host password file %s does not match the user's hash: kling db connect will fail", p)
	}
}

func myClockChecks(ctx context.Context, k klingc.Kling, qr querier, id string, r *report) {
	before := time.Now()
	out, err := k.Run(ctx, nil, "exec", id, "--", "date", "+%s")
	after := time.Now()
	if err != nil {
		checkFailed(r, "MY050", "the guest clock", err)
	} else if g, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); perr != nil {
		checkFailed(r, "MY050", "the guest clock", errors.New("unexpected date output"))
	} else if sk := skew(float64(g), float64(g)+1, before, after); sk > maxSkew {
		r.add("MY050", High, "restart the copy; if it persists the guest agent did not resync the clock after the thaw (see the daemon log)",
			"guest clock is %.1fs off the host clock", sk)
	}
	before = time.Now()
	var ts json.Number
	err = queryJSON(ctx, qr, "", "clock", qMyClock, &ts)
	after = time.Now()
	if err != nil {
		checkFailed(r, "MY050", "the MySQL clock", err)
		return
	}
	f, err := ts.Float64()
	if err != nil {
		checkFailed(r, "MY050", "the MySQL clock", errors.New("unexpected clock output"))
		return
	}
	if sk := skew(f, f, before, after); sk > maxSkew {
		r.add("MY050", High, "restart the copy; the server reads the guest clock, which did not resync after the thaw",
			"MySQL NOW() is %.1fs off the host clock", sk)
	}
}
