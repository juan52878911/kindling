package main

// `kling db tenant-check`: prueba de aislamiento entre inquilinos (clientes)
// dentro de una copia. Nace del fallo real de AuraCRM: políticas RLS con una
// rama "IS NULL OR = ''" que, sin inquilino fijado, dejan ver todas las filas.
//
// El doctor lee las políticas; esto las EJERCITA. Dentro de la copia, por
// `kling exec` y psql por stdin:
//
//  1. Descubre (como superusuario) las tablas con la columna de inquilino
//     (-column, tenant_id), sus políticas y los privilegios del rol de la
//     aplicación (-role; por defecto el de la copia).
//  2. En UNA sesión de psql: guarda en una tabla temporal los primeros -max
//     valores de inquilino y, como el rol (SET ROLE), prueba cada tabla sin
//     inquilino (variable sin fijar y a '') y con cada inquilino: cuántas
//     filas ve y cuántas son de otro, y si puede escribir una fila de otro
//     inquilino (INSERT, mover una suya, UPDATE de las ajenas), siempre en una
//     transacción que se deshace.
//
// Los valores de inquilino NUNCA salen de la base: los lee la propia sesión de
// su tabla temporal (set_config(..., v, ...)); ni pasan por este proceso ni se
// interpolan en la SQL. Tampoco se imprime ninguna fila: solo recuentos y
// SQLSTATE (\set VERBOSITY sqlstate: los errores no citan valores). Los
// nombres de tablas y columnas, que vienen de la base, van citados como
// identificadores; el rol, la columna y la variable son de los flags y pasan
// por un patrón antes de escribirse en la SQL.
//
// SET ROLE en vez de entrar como el rol: las políticas se evalúan con
// current_user, que es el rol; lo que cambia es session_user (postgres) y que
// no se aplican los ALTER ROLE ... SET del rol. Así no hace falta clave ni
// tocar pg_hba.conf.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/plugin"
)

const (
	tcMarker          = "kling-db:tenant-check" // va en lo que se manda (los tests lo buscan)
	defaultTenantCol  = "tenant_id"
	defaultTenantVar  = "app.tenant_id"
	defaultTenantMax  = 5
	maxTenantMax      = 100
	maxTenantTables   = 500
	tcOtherVar        = "kling_tc.other" // el inquilino "ajeno" de las escrituras
	tcTenantsTable    = "pg_temp.kling_tc_tenants"
	tcStatementTmo    = "60s"
	tcLockTmo         = "5s"
	tcScenarioNull    = "null"
	tcScenarioEmpty   = "empty"
	tcTestSelect      = "select"
	tcTestInsert      = "insert-other"
	tcTestMove        = "move-to-other"
	tcTestUpdate      = "update-others"
	tcPass, tcFail    = "pass", "fail"
	tcError, tcSkip   = "error", "skip"
	tcStatusTableFail = "fail"
)

// settingPattern: una variable propia de Postgres lleva un punto (app.tenant_id).
var settingPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,30}\.[a-z_][a-z0-9_]{0,30}$`)

type tcOpts struct {
	role, column, setting string
	max                   int
}

// ── informe ─────────────────────────────────────────────────────────────────

type tcReport struct {
	Copy          string     `json:"copy"`
	Machine       string     `json:"machine"`
	Database      string     `json:"database"`
	Role          string     `json:"role"`
	Column        string     `json:"column"`
	Setting       string     `json:"setting"`
	Max           int        `json:"max"`
	Tenants       int        `json:"tenants"`
	SettingPreset bool       `json:"setting_preset,omitempty"`
	RoleProblem   string     `json:"role_problem,omitempty"`
	Tables        []*tcTable `json:"tables"`
	Failed        int        `json:"failed"`
	Errors        int        `json:"errors"`
	Skipped       int        `json:"skipped"`
	Pass          bool       `json:"pass"`
}

type tcTable struct {
	Schema      string     `json:"schema"`
	Table       string     `json:"table"`
	Status      string     `json:"status"` // pass | fail | error
	RLS         bool       `json:"rls"`
	Force       bool       `json:"force"`
	OwnedByRole bool       `json:"owned_by_role"`
	Privileges  []string   `json:"privileges"`
	Causes      []string   `json:"causes,omitempty"`
	Policies    []tcPolicy `json:"policies"`
	Checks      []tcCheck  `json:"checks"`
	Fix         []string   `json:"fix,omitempty"`

	sel, ins, upd bool
	cols          []string
	bad           string // por qué no se prueba (nombre raro)
	idx           int    // su número en el script (desde 1; 0 = no se prueba)
}

type tcPolicy struct {
	Name       string   `json:"name"`
	Command    string   `json:"command"`
	Permissive bool     `json:"permissive"`
	Roles      []string `json:"roles"`
	Applies    bool     `json:"applies"`
	Using      string   `json:"using,omitempty"`
	WithCheck  string   `json:"with_check,omitempty"`
	Problems   []string `json:"problems,omitempty"`
}

type tcCheck struct {
	Scenario string `json:"scenario"` // null | empty | tenant-N
	Test     string `json:"test"`     // select | insert-other | move-to-other | update-others
	Result   string `json:"result"`   // pass | fail | error | skip
	Rows     int64  `json:"rows,omitempty"`
	SQLState string `json:"sqlstate,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// ── comando ─────────────────────────────────────────────────────────────────

func cmdTenantCheck(args []string) error {
	fs, host, owner := newFlags("tenant-check")
	role := fs.String("role", "", "application role to test as (default: the copy's)")
	column := fs.String("column", defaultTenantCol, "tenant column; every table that has it is checked")
	setting := fs.String("setting", defaultTenantVar, "session setting the RLS policies read (current_setting)")
	max := fs.Int("max", defaultTenantMax, fmt.Sprintf("how many tenant values to test (1-%d)", maxTenantMax))
	asJSON := fs.Bool("json", false, "JSON output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("usage: kling db tenant-check <copy> [-role R] [-column tenant_id] [-setting app.tenant_id] [-max N] [-json]")
	}
	o := tcOpts{role: *role, column: *column, setting: *setting, max: *max}
	if err := o.validate(); err != nil {
		return &plugin.ExitError{Code: 2, Err: err}
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	rep, err := a.tenantCheck(ctx, pos[0], *owner, o)
	if err != nil {
		return err
	}
	if err := writeTenantReport(a.stdout, rep, *asJSON); err != nil {
		return err
	}
	if !rep.Pass {
		return &plugin.ExitError{Code: 1, Err: fmt.Errorf("tenant isolation: %d failed check(s), %d error(s)", rep.Failed, rep.Errors)}
	}
	return nil
}

func (o *tcOpts) validate() error {
	if o.role != "" && (!identPattern.MatchString(o.role) || o.role == "postgres") {
		return fmt.Errorf("invalid -role %q", o.role)
	}
	if !identPattern.MatchString(o.column) {
		return fmt.Errorf("invalid -column %q: a plain lowercase identifier", o.column)
	}
	if !settingPattern.MatchString(o.setting) {
		return fmt.Errorf("invalid -setting %q: prefix.name, lowercase", o.setting)
	}
	if o.setting == tcOtherVar {
		return fmt.Errorf("-setting %s is reserved", tcOtherVar)
	}
	if o.max < 1 || o.max > maxTenantMax {
		return fmt.Errorf("-max must be between 1 and %d", maxTenantMax)
	}
	return nil
}

// tenantCheck prueba el aislamiento en la copia ref (de owner) y devuelve el
// informe. err es para lo que impide probar; los fallos van en el informe.
func (a *app) tenantCheck(ctx context.Context, ref, owner string, o tcOpts) (*tcReport, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	mc, err := a.inspect(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := owned(mc, owner); err != nil {
		return nil, err
	}
	if mc.State != api.StateRunning {
		return nil, fmt.Errorf("%s is %s, not running (kling thaw %s)", mc.Name, mc.State, mc.Name)
	}
	role, db, err := roleDB(mc.Labels)
	if err != nil {
		return nil, err
	}
	if o.role != "" {
		role = o.role
	}
	rep := &tcReport{Copy: mc.Name, Machine: mc.ID, Database: db, Role: role, Column: o.column,
		Setting: o.setting, Max: o.max, Tables: []*tcTable{}}

	// 1. Descubrir.
	out, err := a.k.Run(ctx, strings.NewReader(tcDiscoverSQL(role, o.column)), "exec", "-i", "-timeout", "120s", mc.ID, "--",
		"su", "-s", "/bin/sh", "postgres", "-c", psqlSuperDB(db))
	if err != nil {
		return nil, fmt.Errorf("tenant-check: reading the tables of %s: %w", mc.Name, err)
	}
	if len(out) > maxJSON {
		return nil, errors.New("tenant-check: discovery output too large")
	}
	var d tcDiscovery
	if err := json.Unmarshal([]byte(lastLine(string(out))), &d); err != nil {
		return nil, fmt.Errorf("tenant-check: unexpected discovery output from %s: %w", mc.Name, err)
	}
	if !d.RoleExists {
		return nil, fmt.Errorf("role %q does not exist in %s (use -role)", role, mc.Name)
	}
	if len(d.Tables) == 0 {
		return nil, fmt.Errorf("no table in database %s of %s has a %q column (use -column)", db, mc.Name, o.column)
	}
	if len(d.Tables) > maxTenantTables {
		return nil, fmt.Errorf("%d tables have a %q column; tenant-check handles at most %d", len(d.Tables), o.column, maxTenantTables)
	}
	switch {
	case d.Super:
		rep.RoleProblem = fmt.Sprintf("role %s is SUPERUSER: row level security never applies to it (ALTER ROLE %s NOSUPERUSER)", role, role)
	case d.BypassRLS:
		rep.RoleProblem = fmt.Sprintf("role %s has BYPASSRLS: row level security never applies to it (ALTER ROLE %s NOBYPASSRLS)", role, role)
	}
	var testable []*tcTable
	for _, dt := range d.Tables {
		t := dt.table(o)
		rep.Tables = append(rep.Tables, t)
		if t.bad == "" {
			testable = append(testable, t)
		}
	}

	// 2. Probar, en una sola sesión.
	res := newTCResults()
	if len(testable) > 0 {
		script := tcScript(o, role, testable)
		out, err := a.k.Run(ctx, strings.NewReader(script), "exec", "-i", "-timeout", "600s", mc.ID, "--",
			"su", "-s", "/bin/sh", "postgres", "-c", "psql -X -q -At -d "+db)
		if err != nil {
			return nil, fmt.Errorf("tenant-check: running the checks in %s: %w", mc.Name, err)
		}
		res = parseTCOutput(string(out))
		if !res.done {
			return nil, fmt.Errorf("tenant-check: the checks in %s did not finish", mc.Name)
		}
	}
	rep.Tenants = res.tenants
	rep.SettingPreset = res.preset
	for _, t := range rep.Tables {
		judge(rep, t, res)
	}
	if rep.RoleProblem != "" {
		rep.Failed++
	}
	rep.Pass = rep.Failed == 0 && rep.Errors == 0
	return rep, nil
}

// ── descubrimiento ──────────────────────────────────────────────────────────

type tcDiscovery struct {
	RoleExists bool          `json:"role_exists"`
	Super      bool          `json:"super"`
	BypassRLS  bool          `json:"bypassrls"`
	Tables     []tcDiscTable `json:"tables"`
}

type tcDiscTable struct {
	Schema   string         `json:"schema"`
	Table    string         `json:"tbl"`
	RLS      bool           `json:"rls"`
	Force    bool           `json:"force"`
	Owner    bool           `json:"owner"`
	Sel      bool           `json:"sel"`
	Ins      bool           `json:"ins"`
	Upd      bool           `json:"upd"`
	Cols     []string       `json:"cols"`
	Policies []tcDiscPolicy `json:"policies"`
}

type tcDiscPolicy struct {
	Name       string   `json:"name"`
	Permissive string   `json:"permissive"`
	Cmd        string   `json:"cmd"`
	Roles      []string `json:"roles"`
	Qual       string   `json:"qual"`
	WithCheck  string   `json:"with_check"`
	Applies    bool     `json:"applies"`
}

// tcDiscoverSQL: role y column pasaron identPattern (van como literales sin
// nada que escapar). Sin '%' en la SQL: fmt.
func tcDiscoverSQL(role, column string) string {
	return fmt.Sprintf(`-- `+tcMarker+`:discover
WITH ro AS (SELECT oid, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = '%[1]s')
SELECT jsonb_build_object(
  'role_exists', EXISTS (SELECT 1 FROM ro),
  'super', coalesce((SELECT rolsuper FROM ro), false),
  'bypassrls', coalesce((SELECT rolbypassrls FROM ro), false),
  'tables', coalesce((SELECT jsonb_agg(t ORDER BY t.schema, t.tbl) FROM (
    SELECT n.nspname::text AS schema, c.relname::text AS tbl,
      c.relrowsecurity AS rls, c.relforcerowsecurity AS force,
      coalesce(pg_has_role((SELECT oid FROM ro), c.relowner, 'USAGE'), false) AS owner,
      coalesce(has_table_privilege((SELECT oid FROM ro), c.oid, 'SELECT'), false) AS sel,
      coalesce(has_table_privilege((SELECT oid FROM ro), c.oid, 'INSERT'), false) AS ins,
      coalesce(has_column_privilege((SELECT oid FROM ro), c.oid, a.attnum, 'UPDATE'), false) AS upd,
      (SELECT coalesce(jsonb_agg(x.attname::text ORDER BY x.attnum), '[]'::jsonb) FROM pg_attribute x
        WHERE x.attrelid = c.oid AND x.attnum > 0 AND NOT x.attisdropped AND x.attgenerated = '') AS cols,
      (SELECT coalesce(jsonb_agg(jsonb_build_object(
          'name', p.policyname::text, 'permissive', p.permissive, 'cmd', p.cmd,
          'roles', p.roles::text[], 'qual', coalesce(p.qual, ''), 'with_check', coalesce(p.with_check, ''),
          'applies', coalesce('public' = ANY (p.roles) OR EXISTS (SELECT 1 FROM unnest(p.roles) r
              WHERE r <> 'public' AND pg_has_role((SELECT oid FROM ro), r, 'MEMBER')), false))
          ORDER BY p.policyname), '[]'::jsonb)
        FROM pg_policies p WHERE p.schemaname = n.nspname AND p.tablename = c.relname) AS policies
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    JOIN pg_attribute a ON a.attrelid = c.oid
    WHERE a.attname = '%[2]s' AND NOT a.attisdropped AND c.relkind IN ('r', 'p') AND NOT c.relispartition
      AND n.nspname <> 'information_schema' AND n.nspname !~ '^pg_'
      AND NOT EXISTS (SELECT 1 FROM pg_depend e WHERE e.classid = 'pg_class'::regclass AND e.objid = c.oid AND e.deptype = 'e')
  ) t), '[]'::jsonb))::text;
`, role, column)
}
