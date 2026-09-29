package main

// `kling db role`: roles extra dentro de una copia. Hoy solo el de SOLO
// LECTURA (-ro), pensado para darle un agente o una herramienta de análisis
// sin darle la base entera.
//
// Qué hace que sea de solo lectura de verdad: no es la bandera
// default_transaction_read_only (un cliente puede apagarla con SET), sino que
// el rol NO TIENE privilegios de escritura: solo USAGE en esquemas y SELECT en
// tablas, sin superusuario, sin BYPASSRLS, sin pertenencia a ningún rol (ni
// pg_read_server_files ni pg_execute_server_program). La bandera y los
// tiempos máximos son la segunda capa.
//
// La contraseña sigue la regla del resto de la extensión: se genera en el
// host, al invitado solo va su verificador SCRAM por stdin, y vive solo en
// <estado>/copies/<id>/<rol>.password (0600). Nunca en argv, etiquetas ni
// stdout.
//
// pg_hba.conf de la golden deja entrar por red solo al rol de la aplicación:
// el rol nuevo necesita su propia línea (con SCRAM, solo a la base de la
// copia). Se añade DESPUÉS de comprobar el rol, y se quita al borrarlo o si
// algo falla.
//
// El rol NO pasa a las copias que nacen de esta (fork, undo, un golden hecho
// de ella): prepare lo borra antes de darlas por listas (purgeInheritedRoles).

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/pkg/api"
)

const (
	roleComment    = "kling-db:ro" // marca de los roles que crea este comando
	roleConnLimit  = 5
	defaultRoleRO  = "agent"
	defaultRoleTmo = 5 * time.Second
)

// hbaPrelude lo comparten los scripts que editan pg_hba.conf: define q (una
// consulta como el superusuario, por el socket) y deja en F la ruta que dice
// el propio Postgres (SHOW hba_file), como hace ask. Se exige una ruta
// absoluta sin caracteres raros: va entre comillas, pero es de fuera.
const hbaPrelude = `set -eu
q() { su -s /bin/sh postgres -c "psql -X -q -At -v ON_ERROR_STOP=1 -d postgres -c '$1'"; }
F=$(q 'SHOW hba_file')
case "$F" in
  /*) ;;
  *) echo "unexpected pg_hba path" >&2; exit 1 ;;
esac
case "$F" in
  *[!A-Za-z0-9/._-]*) echo "unexpected pg_hba path" >&2; exit 1 ;;
esac
`

// roleNamePattern: identificador simple, hasta los 63 bytes de Postgres.
var roleNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// reservedRoles son nombres con significado especial en SQL o en pg_hba.conf.
var reservedRoles = map[string]bool{
	"postgres": true, "public": true, "none": true, "all": true, "replication": true,
	"sameuser": true, "samerole": true, "samegroup": true, "session_user": true,
	"current_user": true, "current_role": true, "user": true, "system_user": true,
}

// validRoleName exige un nombre válido que no choque con el rol dueño.
func validRoleName(name, ownerRole string) error {
	if !roleNamePattern.MatchString(name) {
		return fmt.Errorf("invalid role name %q: lowercase letters, digits and '_', starting with a letter or '_', up to 63 characters", name)
	}
	if reservedRoles[name] || strings.HasPrefix(name, "pg_") {
		return fmt.Errorf("role name %q is reserved", name)
	}
	if name == ownerRole {
		return fmt.Errorf("role name %q is the application role of the copy", name)
	}
	return nil
}

// qIdent cita un identificador de SQL.
func qIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func cmdRole(args []string) error {
	fs, host, owner := newFlags("role")
	ro := fs.Bool("ro", false, "a read-only login role")
	name := fs.String("name", defaultRoleRO, "name of the role")
	schemas := fs.String("schemas", "", "comma-separated schemas it can read (default: all but the system ones)")
	timeout := fs.Duration("timeout", defaultRoleTmo, "statement_timeout and idle_in_transaction_session_timeout of the role")
	rm := fs.Bool("rm", false, "remove the role instead of creating it")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || (!*ro && !*rm) {
		return usageErr("usage: kling db role <copy> -ro [-name agent] [-schemas a,b] [-timeout 5s] [-rm]")
	}
	if *rm && (*schemas != "") {
		return usageErr("-rm takes no -schemas")
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	if *rm {
		return a.roleRemove(ctx, pos[0], *owner, *name)
	}
	var list []string
	if *schemas != "" {
		list = strings.Split(*schemas, ",")
	}
	return a.roleCreateRO(ctx, pos[0], *owner, *name, list, *timeout)
}

// sqlSuper corre SQL como superusuario dentro de la copia, por stdin. El error
// no lleva la salida: psql cita la sentencia, y la sentencia puede llevar el
// verificador.
func (a *app) sqlSuper(ctx context.Context, id, sql, what string) (string, error) {
	out, err := a.k.Run(ctx, strings.NewReader(sql), "exec", "-i", "-timeout", "60s", id, "--",
		"su", "-s", "/bin/sh", "postgres", "-c", psqlSuper)
	if err != nil {
		return "", fmt.Errorf("%s failed (output omitted: it may quote the statement)", what)
	}
	return strings.TrimSpace(string(out)), nil
}

// roleTarget es la copia sobre la que se trabaja, ya comprobada.
func (a *app) roleTarget(ctx context.Context, ref, owner, name string) (mc *api.Machine, ownerRole, db string, err error) {
	if err := validOwner(owner); err != nil {
		return nil, "", "", err
	}
	mc, err = a.inspect(ctx, ref)
	if err != nil {
		return nil, "", "", err
	}
	if err := checkReady(mc, owner); err != nil {
		return nil, "", "", err
	}
	if err := requirePostgres(mc, "role"); err != nil {
		return nil, "", "", err
	}
	ownerRole, db, err = roleDB(mc.Labels)
	if err != nil {
		return nil, "", "", err
	}
	if err := validRoleName(name, ownerRole); err != nil {
		return nil, "", "", err
	}
	return mc, ownerRole, db, nil
}

// roleState dice si el rol existe y con qué comentario.
func (a *app) roleState(ctx context.Context, id, name string) (exists bool, comment string, err error) {
	out, err := a.sqlSuper(ctx, id, fmt.Sprintf(
		"SELECT count(*) || ':' || coalesce(max(shobj_description(oid, 'pg_authid')), '') FROM pg_roles WHERE rolname = '%s';\n", name),
		"looking up the role")
	if err != nil {
		return false, "", err
	}
	n, c, ok := strings.Cut(out, ":")
	if !ok || (n != "0" && n != "1") {
		return false, "", fmt.Errorf("looking up the role: unexpected answer %q", out)
	}
	return n == "1", c, nil
}

// hbaLine es la línea de pg_hba.conf del rol: solo su base, por red y con
// SCRAM. dbField va tal cual: la base entre comillas, para que una llamada
// "all" o "replication" sea un nombre y no una palabra clave (igual que las
// del rol, que además validRoleName ya rechaza).
func hbaLine(dbField, name string) string {
	return fmt.Sprintf(`host %s "%s" 0.0.0.0/0 scram-sha-256 # kling-db`, dbField, name)
}

// hbaScript añade o quita la línea del rol en pg_hba.conf. Al quitar también
// se va la de versiones anteriores, que abría todas las bases ("host all").
// db y name pasaron identPattern y roleNamePattern: nada que escapar.
func hbaScript(db, name string, add bool) string {
	var b strings.Builder
	b.WriteString(hbaPrelude)
	fmt.Fprintf(&b, "L='%s'\nO='%s'\n", hbaLine(`"`+db+`"`, name), hbaLine("all", name))
	b.WriteString("T=$(mktemp)\ngrep -vxF -e \"$L\" -e \"$O\" \"$F\" > \"$T\" || true\n")
	if add {
		b.WriteString("printf '%s\\n' \"$L\" >> \"$T\"\n")
	}
	// cat > y no mv: se conserva el dueño y el modo del fichero de Postgres.
	b.WriteString("cat \"$T\" > \"$F\"\nrm -f \"$T\"\n")
	return b.String()
}

func (a *app) hba(ctx context.Context, id, db, name string, add bool) error {
	_, err := a.k.Run(ctx, strings.NewReader(hbaScript(db, name, add)), "exec", "-i", "-timeout", "30s", id, "--", "sh", "-s")
	if err != nil {
		return errors.New("editing pg_hba.conf in the copy failed")
	}
	return nil
}

// roleCreateRO crea el rol de solo lectura. Todo o nada: si algo falla se
// deshace lo hecho (rol, línea de hba, contraseña del host).
func (a *app) roleCreateRO(ctx context.Context, ref, owner, name string, schemas []string, timeout time.Duration) error {
	if timeout < 100*time.Millisecond || timeout > time.Hour {
		return errors.New("-timeout must be between 100ms and 1h")
	}
	mc, ownerRole, db, err := a.roleTarget(ctx, ref, owner, name)
	if err != nil {
		return err
	}
	want, err := parseSchemas(schemas)
	if err != nil {
		return err
	}
	exists, _, err := a.roleState(ctx, mc.ID, name)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("role %s already exists in %s (kling db role %s -ro -name %s -rm removes it)", name, mc.Name, mc.Name, name)
	}
	if _, err := dbstate.ReadRolePassword(mc.ID, name); err == nil {
		return fmt.Errorf("this host still holds a password for role %s of %s (kling db role %s -ro -name %s -rm cleans it)", name, mc.Name, mc.Name, name)
	}

	// Esquemas legibles: los de la base salvo los del sistema.
	out, err := a.sqlSuper(ctx, mc.ID, fmt.Sprintf(
		"\\connect %s\nSELECT nspname FROM pg_namespace WHERE nspname !~ '^pg_' AND nspname <> 'information_schema' AND nspname !~ '[[:cntrl:]]' ORDER BY 1;\n", db),
		"listing the schemas")
	if err != nil {
		return err
	}
	have := map[string]bool{}
	var all []string
	for _, s := range strings.Split(out, "\n") {
		if s = strings.TrimSpace(s); s != "" {
			have[s] = true
			all = append(all, s)
		}
	}
	use := all
	if len(want) > 0 {
		use = want
		for _, s := range want {
			if !have[s] {
				return fmt.Errorf("schema %q does not exist in database %s of %s", s, db, mc.Name)
			}
		}
	}
	if len(use) == 0 {
		return fmt.Errorf("database %s of %s has no schema to grant", db, mc.Name)
	}

	pw, err := generatePassword()
	if err != nil {
		return err
	}
	ver, err := newVerifier(pw)
	if err != nil {
		return err
	}
	// Antes de tocar la base: si lo siguiente falla, lo deshace undo.
	if err := dbstate.WriteRolePassword(mc.ID, name, pw); err != nil {
		return fmt.Errorf("storing the password of role %s: %w", name, err)
	}
	undo := func(cause error) error {
		if uerr := a.dropRole(context.Background(), mc.ID, db, name, true); uerr != nil {
			fmt.Fprintf(a.stderr, "warning: could not undo the role %s in %s: %v\n", name, mc.Name, uerr)
		}
		return fmt.Errorf("role %s not created: %w", name, cause)
	}

	ms := int(timeout / time.Millisecond)
	var b strings.Builder
	q := qIdent(name)
	// name, db y ver pasaron sus patrones (el verificador es base64 con '$' y
	// ':'): nada que escapar dentro de las comillas.
	fmt.Fprintf(&b, "CREATE ROLE %s LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT CONNECTION LIMIT %d PASSWORD '%s';\n", q, roleConnLimit, ver)
	fmt.Fprintf(&b, "COMMENT ON ROLE %s IS '%s';\n", q, roleComment)
	fmt.Fprintf(&b, "ALTER ROLE %s SET default_transaction_read_only = on;\n", q)
	fmt.Fprintf(&b, "ALTER ROLE %s SET statement_timeout = '%dms';\n", q, ms)
	fmt.Fprintf(&b, "ALTER ROLE %s SET idle_in_transaction_session_timeout = '%dms';\n", q, ms)
	fmt.Fprintf(&b, "\\connect %s\n", db)
	fmt.Fprintf(&b, "GRANT CONNECT ON DATABASE %s TO %s;\n", qIdent(db), q)
	for _, s := range use {
		qs := qIdent(s)
		fmt.Fprintf(&b, "GRANT USAGE ON SCHEMA %s TO %s;\n", qs, q)
		fmt.Fprintf(&b, "GRANT SELECT ON ALL TABLES IN SCHEMA %s TO %s;\n", qs, q)
		fmt.Fprintf(&b, "ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA %s GRANT SELECT ON TABLES TO %s;\n", qIdent(ownerRole), qs, q)
	}
	// La comprobación va en la misma sesión: el verificador guardado es el de
	// la clave del host, sin poderes de más y sin pertenencias.
	fmt.Fprintf(&b, "SELECT r.rolpassword = '%s' AND NOT r.rolsuper AND NOT r.rolbypassrls AND NOT r.rolcreatedb AND NOT r.rolcreaterole AND NOT r.rolreplication AND r.rolcanlogin AND r.rolconnlimit = %d"+
		" AND NOT EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.member = r.oid)"+
		" FROM pg_authid r WHERE r.rolname = '%s';\n", ver, roleConnLimit, name)
	res, err := a.sqlSuper(ctx, mc.ID, b.String(), "creating the role")
	if err != nil {
		return undo(err)
	}
	if lastLine(res) != "t" {
		return undo(fmt.Errorf("the role %s does not have the expected attributes or verifier", name))
	}

	// Y solo ahora puede entrar por la red.
	if err := a.hba(ctx, mc.ID, db, name, true); err != nil {
		return undo(err)
	}
	res, err = a.sqlSuper(ctx, mc.ID, fmt.Sprintf(
		"SELECT pg_reload_conf();\nSELECT count(*) FROM pg_hba_file_rules WHERE user_name @> ARRAY['%s'] AND database @> ARRAY['%s'] AND error IS NULL;\n", name, db),
		"reloading pg_hba.conf")
	if err != nil {
		return undo(err)
	}
	if lastLine(res) != "1" {
		return undo(errors.New("pg_hba.conf does not hold a valid rule for the role"))
	}

	fmt.Fprintf(a.stdout, "%s  role %s  read-only  (schemas %s; statement_timeout %s; %d connections max)\n",
		mc.Name, name, strings.Join(use, ","), timeout, roleConnLimit)
	p, _ := dbstate.RolePasswordPath(mc.ID, name)
	fmt.Fprintf(a.stdout, "  password  %s\n  kling db connect %s -role %s [-psql | -dsn]\n", p, mc.Name, name)
	return nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

// parseSchemas valida y ordena -schemas.
func parseSchemas(in []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if !roleNamePattern.MatchString(s) {
			return nil, fmt.Errorf("invalid schema name %q in -schemas", s)
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out, nil
}

// dropRole quita el rol de la base (sus permisos, sus sesiones), su línea de
// hba y su contraseña del host. Es idempotente. force salta la comprobación
// de que el rol lo creó este comando (se usa al deshacer una creación).
func (a *app) dropRole(ctx context.Context, id, db, name string, force bool) error {
	exists, comment, err := a.roleState(ctx, id, name)
	if err != nil {
		return err
	}
	if exists {
		if !force && comment != roleComment {
			return fmt.Errorf("role %s was not created by kling db role: not touching it", name)
		}
		q := qIdent(name)
		sql := fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = '%s';\n\\connect %s\nDROP OWNED BY %s;\n\\connect postgres\nDROP ROLE %s;\n", name, db, q, q)
		if _, err := a.sqlSuper(ctx, id, sql, "dropping the role"); err != nil {
			return err
		}
	}
	if err := a.hba(ctx, id, db, name, false); err != nil {
		return err
	}
	if _, err := a.sqlSuper(ctx, id, "SELECT pg_reload_conf();\n", "reloading pg_hba.conf"); err != nil {
		return err
	}
	return dbstate.RemoveRolePassword(id, name)
}

func (a *app) roleRemove(ctx context.Context, ref, owner, name string) error {
	mc, _, db, err := a.roleTarget(ctx, ref, owner, name)
	if err != nil {
		return err
	}
	if err := a.dropRole(ctx, mc.ID, db, name, false); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%s  role %s removed\n", mc.Name, name)
	return nil
}
