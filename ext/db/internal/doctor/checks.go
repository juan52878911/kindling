package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Comprobaciones A: sobre cualquier Postgres. Cada consulta devuelve UN valor
// (JSON en una línea: jsonb no mete saltos de línea), así da igual que la
// lance psql -At dentro de una copia o pgmini desde el host.

// sqlEnv es el contexto de la revisión.
type sqlEnv struct {
	// local: la base vive en una copia de kling db y se revisa desde dentro
	// (el superusuario de arranque postgres entra solo por peer).
	local bool
	// remote: -url a un host que no es loopback.
	remote bool
	// who es la expresión SQL del rol de la aplicación: un literal ya validado
	// ('app') o current_user.
	who string
	// appRole es su nombre (se rellena con la consulta de ajustes si hace falta).
	appRole string
	// dbs son las bases a revisar (políticas y tablas son de cada base).
	dbs []string
}

const (
	qRoles = `SELECT coalesce(jsonb_agg(t ORDER BY t.name), '[]'::jsonb)::text FROM (
  SELECT r.rolname AS name, r.rolsuper AS super, r.rolbypassrls AS bypassrls,
         r.rolcreaterole AS createrole,
         ARRAY(SELECT d FROM unnest(ARRAY['pg_execute_server_program','pg_read_server_files','pg_write_server_files']) d
               WHERE NOT r.rolsuper AND pg_has_role(r.oid, d, 'MEMBER')) AS dangerous
  FROM pg_roles r WHERE r.rolcanlogin) t`

	qSettings = `SELECT jsonb_build_object(
  'password_encryption', current_setting('password_encryption'),
  'ssl', current_setting('ssl'),
  'user', current_user::text)::text`

	qMD5 = `SELECT coalesce(jsonb_agg(rolname::text ORDER BY rolname), '[]'::jsonb)::text
  FROM pg_authid WHERE rolpassword LIKE 'md5%'`

	// %[1]s es sqlEnv.who.
	qSetRole = `SELECT coalesce(jsonb_agg(t ORDER BY t.name), '[]'::jsonb)::text FROM (
  SELECT r.rolname AS name, r.rolsuper AS super, r.rolbypassrls AS bypassrls,
         r.rolcreaterole AS createrole,
         pg_has_role(r.oid, 'pg_execute_server_program', 'MEMBER')
           OR pg_has_role(r.oid, 'pg_read_server_files', 'MEMBER')
           OR pg_has_role(r.oid, 'pg_write_server_files', 'MEMBER') AS files,
         (SELECT rolsuper FROM pg_roles WHERE rolname = %[1]s) AS me_super
  FROM pg_roles r
  WHERE r.rolname <> %[1]s AND r.rolname NOT LIKE 'pg\_%%'
    AND pg_has_role(%[1]s, r.oid, 'MEMBER')) t`

	qPolicies = `SELECT coalesce(jsonb_agg(t ORDER BY t.schema, t.tbl, t.name), '[]'::jsonb)::text FROM (
  SELECT schemaname::text AS schema, tablename::text AS tbl, policyname::text AS name,
         permissive, cmd, coalesce(qual, '') AS qual, coalesce(with_check, '') AS with_check
  FROM pg_policies) t`

	qTenantTables = `SELECT coalesce(jsonb_agg(t ORDER BY t.schema, t.tbl), '[]'::jsonb)::text FROM (
  SELECT n.nspname::text AS schema, c.relname::text AS tbl, c.relrowsecurity AS rls,
         c.relforcerowsecurity AS force, pg_get_userbyid(c.relowner)::text AS owner
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
  JOIN pg_attribute a ON a.attrelid = c.oid
  WHERE a.attname = 'tenant_id' AND NOT a.attisdropped AND c.relkind IN ('r', 'p')
    AND n.nspname NOT IN ('pg_catalog', 'information_schema')
    AND n.nspname NOT LIKE 'pg\_toast%') t`
)

type roleRow struct {
	Name       string   `json:"name"`
	Super      bool     `json:"super"`
	BypassRLS  bool     `json:"bypassrls"`
	CreateRole bool     `json:"createrole"`
	Dangerous  []string `json:"dangerous"`
	Files      bool     `json:"files"`
	MeSuper    bool     `json:"me_super"`
}

type settingsRow struct {
	PasswordEncryption string `json:"password_encryption"`
	SSL                string `json:"ssl"`
	User               string `json:"user"`
}

type policyRow struct {
	Schema     string `json:"schema"`
	Table      string `json:"tbl"`
	Name       string `json:"name"`
	Permissive string `json:"permissive"`
	Cmd        string `json:"cmd"`
	Qual       string `json:"qual"`
	WithCheck  string `json:"with_check"`
}

type tableRow struct {
	Schema string `json:"schema"`
	Table  string `json:"tbl"`
	RLS    bool   `json:"rls"`
	Force  bool   `json:"force"`
	Owner  string `json:"owner"`
}

// queryJSON lanza una consulta y decodifica su único valor.
func queryJSON(ctx context.Context, qr querier, db, name, sql string, v any) error {
	out, err := qr.query(ctx, db, name, sql)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), v); err != nil {
		return fmt.Errorf("%s: unexpected output: %w", name, err)
	}
	return nil
}

// checkFailed deja constancia de una comprobación que no pudo correr.
func checkFailed(r *report, rule, what string, err error) {
	r.add(rule, Warn, "run the doctor with a role that can read the catalog, or check the error",
		"could not check %s: %s", what, safe(err.Error(), 200))
}

func sqlChecks(ctx context.Context, qr querier, env *sqlEnv, r *report) {
	var st settingsRow
	if err := queryJSON(ctx, qr, env.dbs[0], "settings", qSettings, &st); err != nil {
		checkFailed(r, "DB030", "server settings", err)
	} else {
		if env.appRole == "" {
			env.appRole = st.User
		}
		if st.PasswordEncryption != "scram-sha-256" {
			r.add("DB030", Warn, "ALTER SYSTEM SET password_encryption = 'scram-sha-256'; SELECT pg_reload_conf(); then reset the passwords",
				"password_encryption is %s: new passwords are not stored as SCRAM-SHA-256", q(st.PasswordEncryption))
		}
		if env.remote && st.SSL != "on" {
			r.add("DB040", High, "enable ssl on the server (ssl = on, with a certificate) and connect with sslmode=verify-full",
				"ssl is %s on a non-loopback server: traffic, including query data, travels in cleartext", q(st.SSL))
		}
	}

	var roles []roleRow
	appSuper := false
	if err := queryJSON(ctx, qr, env.dbs[0], "roles", qRoles, &roles); err != nil {
		checkFailed(r, "DB001", "login roles", err)
	}
	for _, ro := range roles {
		if ro.Name == env.appRole {
			appSuper = ro.Super
		}
		if ro.Super && env.local && ro.Name == "postgres" {
			r.add("DB001", Info, "",
				"bootstrap superuser \"postgres\" can log in (only over the local socket with peer auth in a kling db copy)")
			continue
		}
		if ro.Super {
			r.add("DB001", Critical, fmt.Sprintf("ALTER ROLE %s NOSUPERUSER; give the application a role that owns only its schema", q(ro.Name)),
				"login role %s is SUPERUSER: it bypasses RLS and can run programs on the server (COPY ... TO PROGRAM)", q(ro.Name))
		}
		if ro.BypassRLS {
			r.add("DB002", Critical, fmt.Sprintf("ALTER ROLE %s NOBYPASSRLS", q(ro.Name)),
				"login role %s has BYPASSRLS: row level security does not apply to it", q(ro.Name))
		}
		if ro.CreateRole && !ro.Super {
			r.add("DB003", High, fmt.Sprintf("ALTER ROLE %s NOCREATEROLE; manage roles from a separate admin role", q(ro.Name)),
				"login role %s has CREATEROLE: it can create and grant roles", q(ro.Name))
		}
		for _, d := range ro.Dangerous {
			sev := Critical
			if d == "pg_read_server_files" {
				sev = High
			}
			r.add("DB004", sev, fmt.Sprintf("REVOKE %s FROM %s", q(d), q(ro.Name)),
				"login role %s is a member of %s: it reaches the server's files or programs", q(ro.Name), q(d))
		}
	}

	var md5 []string
	if err := queryJSON(ctx, qr, env.dbs[0], "md5", qMD5, &md5); err != nil {
		r.add("DB031", Info, "", "md5 passwords not checked: pg_authid is not readable by this role")
	} else if len(md5) > 0 {
		names := make([]string, len(md5))
		for i, n := range md5 {
			names[i] = q(n)
		}
		r.add("DB031", Warn, "with password_encryption = 'scram-sha-256', set the password again (ALTER ROLE ... PASSWORD)",
			"roles with an md5 password: %s", strings.Join(names, ", "))
	}

	if env.who != "" && !appSuper {
		var alts []roleRow
		if err := queryJSON(ctx, qr, env.dbs[0], "setrole", fmt.Sprintf(qSetRole, env.who), &alts); err != nil {
			checkFailed(r, "DB020", "SET ROLE targets", err)
		}
		for _, a := range alts {
			if a.MeSuper {
				break
			}
			var why []string
			for _, c := range []struct {
				on   bool
				what string
			}{{a.Super, "SUPERUSER"}, {a.BypassRLS, "BYPASSRLS"}, {a.CreateRole, "CREATEROLE"}, {a.Files, "server file/program access"}} {
				if c.on {
					why = append(why, c.what)
				}
			}
			if len(why) > 0 {
				r.add("DB020", High, fmt.Sprintf("REVOKE %s FROM %s", q(a.Name), q(env.appRole)),
					"application role %s can SET ROLE %s, which has %s", q(env.appRole), q(a.Name), strings.Join(why, ", "))
			}
		}
	}

	vars := map[string]bool{}
	for _, db := range env.dbs {
		policyChecks(ctx, qr, db, env, r, vars)
	}
	names := make([]string, 0, len(vars))
	for v := range vars {
		names = append(names, v)
	}
	sort.Strings(names)
	for _, v := range names {
		r.add("DB021", Info, "",
			"any role, including %s, can SET %s itself: tenant isolation depends on the application never passing untrusted input to SET or set_config",
			q(env.appRole), q(v))
	}
}

func policyChecks(ctx context.Context, qr querier, db string, env *sqlEnv, r *report, vars map[string]bool) {
	where := ""
	if len(env.dbs) > 1 {
		where = " in database " + q(db)
	}
	var pols []policyRow
	if err := queryJSON(ctx, qr, db, "policies", qPolicies, &pols); err != nil {
		checkFailed(r, "DB010", "RLS policies"+where, err)
	}
	for _, p := range pols {
		for _, e := range []struct{ kind, expr string }{{"USING", p.Qual}, {"WITH CHECK", p.WithCheck}} {
			for _, v := range tenantVars(e.expr) {
				vars[v] = true
			}
			if why, bad := failOpen(e.expr); bad {
				r.addObj("DB010", Critical, FailOpenFix, "", safe(p.Schema, 64)+"."+safe(p.Table, 64),
					"policy %s on %s.%s%s is fail-open in %s: %s, so every row is visible without a tenant; expression: %s",
					q(p.Name), q(p.Schema), q(p.Table), where, e.kind, safe(why, 120), safe(e.expr, 160))
			}
		}
	}

	var tabs []tableRow
	if err := queryJSON(ctx, qr, db, "tenant_tables", qTenantTables, &tabs); err != nil {
		checkFailed(r, "DB011", "tenant tables"+where, err)
	}
	for _, t := range tabs {
		name := q(t.Schema) + "." + q(t.Table)
		switch {
		case !t.RLS:
			r.addObj("DB011", High, fmt.Sprintf("ALTER TABLE %s ENABLE ROW LEVEL SECURITY; and add a fail-closed policy", name),
				"ALTER TABLE <table> ENABLE ROW LEVEL SECURITY on each one, and add a fail-closed policy (better: in the migration that creates it)",
				safe(t.Schema, 64)+"."+safe(t.Table, 64),
				"table %s%s has a tenant_id column but row level security is off", name, where)
		case !t.Force && t.Owner == env.appRole:
			r.addObj("DB012", High, fmt.Sprintf("ALTER TABLE %s FORCE ROW LEVEL SECURITY", name),
				"ALTER TABLE <table> FORCE ROW LEVEL SECURITY on each one", safe(t.Schema, 64)+"."+safe(t.Table, 64),
				"table %s%s is owned by the application role %s without FORCE ROW LEVEL SECURITY: the owner skips its policies",
				name, where, q(t.Owner))
		}
	}
}
