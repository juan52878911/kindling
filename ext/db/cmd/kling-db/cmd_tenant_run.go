package main

// kling db tenant-check (2/2): el script de las pruebas, su salida, el
// veredicto y el informe. Ver cmd_tenant.go.

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/juan52878911/kindling/ext/db/internal/doctor"
)

// table pasa una tabla descubierta al informe, con el análisis estático de sus
// políticas (las reglas del doctor).
func (dt *tcDiscTable) table(o tcOpts) *tcTable {
	t := &tcTable{Schema: dt.Schema, Table: dt.Table, RLS: dt.RLS, Force: dt.Force, OwnedByRole: dt.Owner,
		Privileges: []string{}, Policies: []tcPolicy{}, Checks: []tcCheck{},
		sel: dt.Sel, ins: dt.Ins, upd: dt.Upd, cols: dt.Cols}
	for _, p := range []struct {
		ok   bool
		name string
	}{{dt.Sel, "SELECT"}, {dt.Ins, "INSERT"}, {dt.Upd, "UPDATE"}} {
		if p.ok {
			t.Privileges = append(t.Privileges, p.name)
		}
	}
	if !plainName(dt.Schema) || !plainName(dt.Table) {
		t.bad = "its name has control characters: not checked"
	}
	for _, c := range dt.Cols {
		if !plainName(c) {
			t.cols = nil // sin la prueba de INSERT, que nombra cada columna
			break
		}
	}
	for _, p := range dt.Policies {
		pol := tcPolicy{Name: p.Name, Command: p.Cmd, Permissive: p.Permissive != "RESTRICTIVE",
			Roles: p.Roles, Applies: p.Applies, Using: doctor.Safe(p.Qual, 300), WithCheck: doctor.Safe(p.WithCheck, 300)}
		if pol.Roles == nil {
			pol.Roles = []string{}
		}
		if p.Applies {
			pol.Problems = policyProblems(p, pol.Permissive, o.setting)
		}
		t.Policies = append(t.Policies, pol)
	}
	return t
}

// policyProblems: por qué una política que se aplica al rol no separa
// inquilinos. Las reglas de fail-open son las del doctor (DB010).
func policyProblems(p tcDiscPolicy, permissive bool, setting string) []string {
	var out []string
	for _, e := range []struct{ kind, expr string }{{"USING", p.Qual}, {"WITH CHECK", p.WithCheck}} {
		if strings.TrimSpace(e.expr) == "" {
			continue
		}
		if why, open := doctor.FailOpen(e.expr); open {
			out = append(out, fmt.Sprintf("fail-open in %s: %s, so with no tenant set every row passes", e.kind, doctor.Safe(why, 120)))
			continue
		}
		if !permissive {
			continue
		}
		if isTrueExpr(e.expr) {
			out = append(out, fmt.Sprintf("%s (true): every row passes", e.kind))
			continue
		}
		reads := false
		for _, v := range doctor.TenantVars(e.expr) {
			reads = reads || v == setting
		}
		if !reads {
			out = append(out, fmt.Sprintf("%s does not read %s: it does not separate tenants", e.kind, setting))
		}
	}
	return out
}

// isTrueExpr: "true", "(true)", "( TRUE )".
func isTrueExpr(e string) bool {
	e = strings.ToLower(strings.TrimSpace(e))
	for strings.HasPrefix(e, "(") && strings.HasSuffix(e, ")") {
		e = strings.TrimSpace(e[1 : len(e)-1])
	}
	return e == "true"
}

// plainName: un nombre de la base que se puede citar en un script de psql y
// enseñar sin más: no vacío y sin caracteres de control.
func plainName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) || c == unicode.ReplacementChar {
			return false
		}
	}
	return true
}

// sqlName cita schema.tabla con qIdent (cmd_role.go). psql no interpreta nada
// dentro de un identificador citado (ni :variables ni \).
func (t *tcTable) sqlName() string { return qIdent(t.Schema) + "." + qIdent(t.Table) }

func (t *tcTable) display() string { return doctor.Safe(t.Schema, 64) + "." + doctor.Safe(t.Table, 64) }

// ── el script de las pruebas ────────────────────────────────────────────────

// tcScript escribe la sesión de psql de las pruebas y numera las tablas
// (t.idx). Cada prueba va precedida de "-- kt <prueba> <escenario> <tabla>" y
// deja una línea en stdout (ver parseTCOutput).
//
// Lo que prepara (tabla de inquilinos, set_config, SET ROLE) va con
// ON_ERROR_STOP: si falla, psql acaba sin DONE y no hay informe a medias. Las
// pruebas no: un error es un resultado (su SQLSTATE).
func tcScript(o tcOpts, role string, tabs []*tcTable) string {
	var b strings.Builder
	w := func(format string, a ...any) {
		fmt.Fprintf(&b, format, a...)
		b.WriteByte('\n')
	}
	col := qIdent(o.column)
	setting := "'" + o.setting + "'" // settingPattern: nada que escapar
	w("-- %s:run", tcMarker)
	w(`\set ON_ERROR_STOP on`)
	w(`\set VERBOSITY sqlstate`)
	w(`\set SHOW_CONTEXT never`)
	w("SET statement_timeout = '%s';", tcStatementTmo)
	w("SET lock_timeout = '%s';", tcLockTmo)
	w("SET row_security = on;")
	w("SELECT 'U ' || (current_setting(%s, true) IS NULL)::text;", setting)
	// Los inquilinos: leídos como superusuario (sin RLS) de todas las tablas.
	// Se quedan en la sesión: ni salen de la base ni se escriben en la SQL.
	w("CREATE TEMP TABLE kling_tc_tenants AS SELECT row_number() OVER (ORDER BY v) AS n, v FROM (")
	w("  SELECT DISTINCT v FROM (")
	for j, t := range tabs {
		t.idx = j + 1
		sep := "   "
		if j > 0 {
			sep = "    UNION ALL"
		}
		w("%s SELECT %s::text AS v FROM %s", sep, col, t.sqlName())
	}
	w("  ) u WHERE v IS NOT NULL AND v <> '' ORDER BY v LIMIT %d) s;", o.max)
	w(`SELECT count(*) AS n FROM %s \gset kt_`, tcTenantsTable)
	w(`\echo T :kt_n`)

	scenario := func(tag string, setup ...string) {
		for _, s := range setup {
			w("%s", s)
		}
		w("SET ROLE %s;", qIdent(role))
		w(`\set ON_ERROR_STOP off`)
		for _, t := range tabs {
			tcChecks(w, t, tag, role, col, setting)
		}
		w(`\set ON_ERROR_STOP on`)
		w("RESET ROLE;")
	}

	// Sin inquilino: la variable sin fijar (la sesión no la ha tocado) y a ''.
	scenario(tcScenarioNull)
	scenario(tcScenarioEmpty, fmt.Sprintf(`SELECT set_config(%s, '', false) IS NOT NULL AS ok \gset kt_`, setting))

	// Con cada inquilino.
	for i := 1; i <= o.max; i++ {
		w(`SELECT count(*) >= %d AS has FROM %s \gset kt_`, i, tcTenantsTable)
		w(`\if :kt_has`)
		scenario("t"+strconv.Itoa(i),
			fmt.Sprintf(`SELECT set_config(%s, v, false) IS NOT NULL AS ok FROM %s WHERE n = %d \gset kt_`, setting, tcTenantsTable, i))
		w(`\endif`)
	}
	w(`\echo DONE`)
	return b.String()
}

// tcChecks escribe las pruebas de una tabla en un escenario (ya como el rol).
func tcChecks(w func(string, ...any), t *tcTable, tag, role, col, setting string) {
	if !t.sel {
		return
	}
	name := t.sqlName()
	j := t.idx
	w("-- kt %s %s %d", tcTestSelect, tag, j)
	w("SELECT concat_ws(' ', 'S', '%s', %d, count(*), count(*) FILTER (WHERE %s::text IS DISTINCT FROM current_setting(%s, true))) FROM %s;",
		tag, j, col, setting, name)
	w(`\if :ERROR`)
	w(`\echo E %s %d :SQLSTATE`, tag, j)
	w(`\endif`)
	if !t.ins && !t.upd {
		return
	}

	// Escrituras. El "otro" inquilino es un valor de ESTA tabla distinto del
	// fijado: lo lee el superusuario (sin RLS) a una variable de la sesión, y
	// el rol lo toma de ahí. Sin ese valor no hay escrituras que probar.
	w(`\set ON_ERROR_STOP on`)
	w("RESET ROLE;")
	w(`SELECT set_config('%s', coalesce((SELECT %s::text FROM %s WHERE %s IS NOT NULL AND %s::text IS DISTINCT FROM current_setting(%s, true) LIMIT 1), ''), false) <> '' AS other \gset kt_`,
		tcOtherVar, col, name, col, col, setting)
	w("SET ROLE %s;", qIdent(role))
	w(`\set ON_ERROR_STOP off`)
	w(`\if :kt_other`)
	w(`\echo O %s %d`, tag, j)
	// Siempre en una transacción que se deshace. El valor se convierte al tipo
	// de la columna sin nombrar el tipo: json_populate_record sobre el tipo
	// fila de la tabla. La clave del JSON es la columna, que pasó identPattern.
	colKey := strings.Trim(col, `"`)
	otherVal := fmt.Sprintf("json_build_object('%s', current_setting('%s'))", colKey, tcOtherVar)
	if t.ins && len(t.cols) > 0 {
		cols := make([]string, len(t.cols))
		sel := make([]string, len(t.cols))
		for i, c := range t.cols {
			cols[i] = qIdent(c)
			sel[i] = "(x)." + qIdent(c)
		}
		w("-- kt %s %s %d", tcTestInsert, tag, j)
		w("BEGIN;")
		w("INSERT INTO %s (%s) OVERRIDING SYSTEM VALUE SELECT %s FROM (SELECT json_populate_record(r, %s) AS x FROM %s AS r LIMIT 1) AS s;",
			name, strings.Join(cols, ", "), strings.Join(sel, ", "), otherVal, name)
		w(`\echo W %s %s %d :SQLSTATE :ROW_COUNT`, tcTestInsert, tag, j)
		w("ROLLBACK;")
	}
	if t.upd {
		w("-- kt %s %s %d", tcTestMove, tag, j)
		w("BEGIN;")
		w("UPDATE %s SET %s = (json_populate_record(NULL::%s, %s)).%s WHERE ctid IN (SELECT ctid FROM %s LIMIT 1);",
			name, col, name, otherVal, col, name)
		w(`\echo W %s %s %d :SQLSTATE :ROW_COUNT`, tcTestMove, tag, j)
		w("ROLLBACK;")
		w("-- kt %s %s %d", tcTestUpdate, tag, j)
		w("BEGIN;")
		w("UPDATE %s SET %s = %s WHERE %s::text IS DISTINCT FROM current_setting(%s, true);", name, col, col, col, setting)
		w(`\echo W %s %s %d :SQLSTATE :ROW_COUNT`, tcTestUpdate, tag, j)
		w("ROLLBACK;")
	}
	w(`\endif`)
}

// ── la salida ───────────────────────────────────────────────────────────────

type tcSelRow struct{ vis, foreign int64 }

type tcWriteRow struct {
	code string
	rows int64
}

type tcResults struct {
	done, preset bool
	tenants      int
	sel          map[string]tcSelRow   // "escenario tabla"
	err          map[string]string     // "escenario tabla" -> SQLSTATE del SELECT
	write        map[string]tcWriteRow // "prueba escenario tabla"
	other        map[string]bool       // "escenario tabla": hubo otro inquilino con el que escribir
}

func newTCResults() *tcResults {
	return &tcResults{sel: map[string]tcSelRow{}, err: map[string]string{}, write: map[string]tcWriteRow{}, other: map[string]bool{}}
}

var sqlstatePattern = regexp.MustCompile(`^[0-9A-Z]{5}$`)

// parseTCOutput lee las líneas del script; lo que no reconoce se ignora.
func parseTCOutput(out string) *tcResults {
	r := newTCResults()
	num := func(s string) (int64, bool) {
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil && n >= 0
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch {
		case f[0] == "DONE" && len(f) == 1:
			r.done = true
		case f[0] == "U" && len(f) == 2:
			r.preset = f[1] == "false"
		case f[0] == "T" && len(f) == 2:
			if n, ok := num(f[1]); ok && n <= maxTenantMax {
				r.tenants = int(n)
			}
		case f[0] == "S" && len(f) == 5:
			v, ok1 := num(f[3])
			fo, ok2 := num(f[4])
			if ok1 && ok2 {
				r.sel[f[1]+" "+f[2]] = tcSelRow{vis: v, foreign: fo}
			}
		case f[0] == "O" && len(f) == 3:
			r.other[f[1]+" "+f[2]] = true
		case f[0] == "E" && len(f) == 4 && sqlstatePattern.MatchString(f[3]):
			r.err[f[1]+" "+f[2]] = f[3]
		case f[0] == "W" && len(f) == 6 && sqlstatePattern.MatchString(f[4]):
			if n, ok := num(f[5]); ok {
				r.write[f[1]+" "+f[2]+" "+f[3]] = tcWriteRow{code: f[4], rows: n}
			}
		}
	}
	return r
}

// ── veredicto ───────────────────────────────────────────────────────────────

func scenarioName(tag string) string {
	if n, ok := strings.CutPrefix(tag, "t"); ok {
		return "tenant-" + n
	}
	return tag
}

func scenarioText(scen string) string {
	switch scen {
	case tcScenarioNull:
		return "no tenant (setting unset)"
	case tcScenarioEmpty:
		return "no tenant (setting = '')"
	}
	return strings.Replace(scen, "tenant-", "tenant #", 1)
}

func noTenant(tag string) bool { return tag == tcScenarioNull || tag == tcScenarioEmpty }

func classifySelect(tag string, row tcSelRow, ok bool, code string) tcCheck {
	c := tcCheck{Scenario: scenarioName(tag), Test: tcTestSelect}
	switch {
	case code != "" && noTenant(tag):
		c.Result, c.SQLState, c.Detail = tcPass, code, "fails closed (error "+code+")"
	case strings.HasPrefix(code, "22"):
		// Un valor de inquilino de otra tabla que no cabe en el cast de esta.
		c.Result, c.SQLState, c.Detail = tcSkip, code, "inconclusive: this tenant value does not fit the policy's cast (error "+code+")"
	case code != "":
		c.Result, c.SQLState, c.Detail = tcError, code, "the role cannot read the table with a tenant set (error "+code+")"
	case !ok:
		c.Result, c.Detail = tcError, "no result: the check did not run"
	case noTenant(tag) && row.vis > 0:
		c.Result, c.Rows, c.Detail = tcFail, row.vis, fmt.Sprintf("SELECT sees %d row(s) with no tenant set", row.vis)
	case !noTenant(tag) && row.foreign > 0:
		c.Result, c.Rows, c.Detail = tcFail, row.foreign, fmt.Sprintf("SELECT sees %d row(s) of other tenants", row.foreign)
	default:
		c.Result, c.Rows = tcPass, row.vis
	}
	return c
}

func classifyWrite(test, tag string, wr tcWriteRow, ok bool) tcCheck {
	c := tcCheck{Scenario: scenarioName(tag), Test: test}
	what := map[string]string{
		tcTestInsert: "INSERT of a row for another tenant",
		tcTestMove:   "UPDATE moving a row to another tenant",
		tcTestUpdate: "UPDATE of rows of other tenants",
	}[test]
	if noTenant(tag) && test == tcTestUpdate {
		what = "UPDATE of rows with no tenant set"
	}
	switch {
	case !ok:
		c.Result, c.Detail = tcError, "no result: the check did not run"
	case wr.code == "00000" && wr.rows > 0:
		c.Result, c.Rows, c.Detail = tcFail, wr.rows, fmt.Sprintf("%s succeeded (%d row(s), rolled back)", what, wr.rows)
	case wr.code == "00000" && (test == tcTestUpdate || noTenant(tag)):
		c.Result, c.Detail = tcPass, "no row reachable"
	case wr.code == "00000":
		c.Result, c.Detail = tcSkip, "no visible row to try with"
	case wr.code == "42501":
		c.Result, c.SQLState, c.Detail = tcPass, wr.code, "rejected by row level security"
	case strings.HasPrefix(wr.code, "23"):
		// Postgres comprueba la política (WITH CHECK) antes que las
		// restricciones: si salta una, la política dejó pasar la fila.
		c.Result, c.SQLState, c.Detail = tcFail, wr.code,
			fmt.Sprintf("%s passed row level security; only a constraint stopped it (%s)", what, wr.code)
	case strings.HasPrefix(wr.code, "22") || wr.code == "42704" || wr.code == "P0001":
		c.Result, c.SQLState, c.Detail = tcPass, wr.code, "fails closed (error "+wr.code+")"
	default:
		c.Result, c.SQLState, c.Detail = tcSkip, wr.code, "inconclusive (error "+wr.code+")"
	}
	return c
}

// judge da el veredicto de una tabla a partir de la salida y, si falla,
// explica por qué con lo que se sabe de ella (RLS, dueño, políticas).
func judge(rep *tcReport, t *tcTable, res *tcResults) {
	add := func(c tcCheck) {
		t.Checks = append(t.Checks, c)
		switch c.Result {
		case tcFail:
			rep.Failed++
		case tcError:
			rep.Errors++
		case tcSkip:
			rep.Skipped++
		}
	}
	switch {
	case t.bad != "":
		add(tcCheck{Scenario: "all", Test: tcTestSelect, Result: tcError, Detail: t.bad})
	case !t.sel:
		add(tcCheck{Scenario: "all", Test: tcTestSelect, Result: tcSkip, Detail: "the role has no SELECT privilege: nothing to test"})
	default:
		tags := []string{tcScenarioNull, tcScenarioEmpty}
		for i := 1; i <= res.tenants; i++ {
			tags = append(tags, "t"+strconv.Itoa(i))
		}
		for _, tag := range tags {
			k := tag + " " + strconv.Itoa(t.idx)
			row, ok := res.sel[k]
			add(classifySelect(tag, row, ok, res.err[k]))
			if !t.ins && !t.upd {
				continue
			}
			if !res.other[k] {
				add(tcCheck{Scenario: scenarioName(tag), Test: "writes", Result: tcSkip,
					Detail: "no value of another tenant in this table: write checks not run"})
				continue
			}
			if t.ins && len(t.cols) > 0 {
				wr, ok := res.write[tcTestInsert+" "+k]
				add(classifyWrite(tcTestInsert, tag, wr, ok))
			}
			if t.upd {
				for _, test := range []string{tcTestMove, tcTestUpdate} {
					wr, ok := res.write[test+" "+k]
					add(classifyWrite(test, tag, wr, ok))
				}
			}
		}
	}

	t.Status = tcPass
	for _, c := range t.Checks {
		if c.Result == tcFail {
			t.Status = tcStatusTableFail
			break
		}
		if c.Result == tcError {
			t.Status = tcError
		}
	}
	if t.Status != tcStatusTableFail {
		return
	}
	explain(rep, t)
}

// explain rellena el porqué y el arreglo de una tabla que falla.
func explain(rep *tcReport, t *tcTable) {
	name := qIdent(doctor.Safe(t.Schema, 64)) + "." + qIdent(doctor.Safe(t.Table, 64))
	if rep.RoleProblem != "" {
		t.Causes = append(t.Causes, rep.RoleProblem)
	}
	if !t.RLS {
		t.Causes = append(t.Causes, "row level security is off: every row is visible")
		t.Fix = append(t.Fix, fmt.Sprintf("ALTER TABLE %s ENABLE ROW LEVEL SECURITY; and add a fail-closed policy", name))
	} else if t.OwnedByRole && !t.Force {
		t.Causes = append(t.Causes, fmt.Sprintf("role %s owns the table and it has no FORCE ROW LEVEL SECURITY: the owner skips its policies", rep.Role))
		t.Fix = append(t.Fix, fmt.Sprintf("ALTER TABLE %s FORCE ROW LEVEL SECURITY", name))
	}
	failOpen, loose := false, false
	for _, p := range t.Policies {
		kind := "restrictive"
		if p.Permissive {
			kind = "permissive"
		}
		for _, pr := range p.Problems {
			t.Causes = append(t.Causes, fmt.Sprintf("policy %s (%s, %s): %s", qIdent(doctor.Safe(p.Name, 64)), kind, p.Command, pr))
			if strings.HasPrefix(pr, "fail-open") {
				failOpen = true
			} else {
				loose = true
			}
		}
	}
	if failOpen {
		t.Fix = append(t.Fix, doctor.FailOpenFix)
	}
	if loose {
		t.Fix = append(t.Fix, fmt.Sprintf("restrict the policy to %s = current_setting('%s')::<type of %s>", rep.Column, rep.Setting, rep.Column))
	}
	if len(t.Causes) == 0 {
		t.Causes = append(t.Causes, "no single policy explains it: look at how the permissive policies combine, and at the views or SECURITY DEFINER functions they call")
	}
}

// ── el informe ──────────────────────────────────────────────────────────────

// tcMaxLines acota las pruebas que se enseñan por tabla en el texto.
const tcMaxLines = 8

func writeTenantReport(w io.Writer, rep *tcReport, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	fmt.Fprintf(w, "kling db tenant-check: %s (database %s, role %s, column %s, setting %s)\n",
		doctor.Safe(rep.Copy, 64), rep.Database, rep.Role, rep.Column, rep.Setting)
	fmt.Fprintf(w, "  tenants tested: %d (-max %d)\n", rep.Tenants, rep.Max)
	if rep.Tenants == 0 {
		fmt.Fprintln(w, "  note: no tenant values found: only the no-tenant cases ran")
	}
	if rep.SettingPreset {
		fmt.Fprintf(w, "  note: %s already has a value when a session starts (ALTER DATABASE/ROLE ... SET or postgresql.conf): the \"setting unset\" case tests that value\n", rep.Setting)
	}
	if rep.RoleProblem != "" {
		fmt.Fprintf(w, "  FAIL   %s\n", rep.RoleProblem)
	}
	tabs := append([]*tcTable(nil), rep.Tables...)
	order := map[string]int{tcStatusTableFail: 0, tcError: 1, tcPass: 2}
	sort.SliceStable(tabs, func(i, j int) bool { return order[tabs[i].Status] < order[tabs[j].Status] })
	failing := 0
	for _, t := range tabs {
		rls := "RLS off"
		if t.RLS {
			rls = "RLS on"
			if t.Force {
				rls += ", forced"
			}
		}
		pols := "policies"
		if len(t.Policies) == 1 {
			pols = "policy"
		}
		fmt.Fprintf(w, "  %-6s %s  (%s, %d %s)\n", strings.ToUpper(t.Status), t.display(), rls, len(t.Policies), pols)
		if t.Status != tcPass {
			failing++
		}
		shown, hidden := 0, 0
		for _, c := range t.Checks {
			if c.Result == tcPass || (c.Result == tcSkip && c.SQLState == "") {
				continue
			}
			if shown == tcMaxLines {
				hidden++
				continue
			}
			shown++
			fmt.Fprintf(w, "         %-5s %s: %s\n", c.Result, scenarioText(c.Scenario), c.Detail)
		}
		if hidden > 0 {
			fmt.Fprintf(w, "         ... and %d more (-json has them all)\n", hidden)
		}
		for _, c := range t.Causes {
			fmt.Fprintf(w, "         why:  %s\n", c)
		}
		for _, f := range t.Fix {
			fmt.Fprintf(w, "         fix:  %s\n", f)
		}
	}
	fmt.Fprintf(w, "summary: %d table(s), %d not passing; %d failed check(s), %d error(s), %d skipped\n",
		len(rep.Tables), failing, rep.Failed, rep.Errors, rep.Skipped)
	return nil
}
