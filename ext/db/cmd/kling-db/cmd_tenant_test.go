package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// tenantKling envuelve al falso del daemon y contesta a las dos sesiones de
// psql de tenant-check: el descubrimiento (JSON fijo) y las pruebas, que
// simula a partir de las marcas "-- kt" del script y de un modelo: unas
// tablas con filas de unos inquilinos y una política que, sin inquilino,
// deja ver todo (failOpen, como AuraCRM) o falla cerrada.
type tenantKling struct {
	*fakeKling
	disc     string
	tenants  []string // valores de inquilino "dentro" de la base
	rows     int      // filas de cada inquilino en cada tabla
	failOpen bool
	scripts  []string
}

var ktMarker = regexp.MustCompile(`^-- kt (\S+) (\S+) (\d+)$`)

func (k *tenantKling) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	if len(args) == 0 || args[0] != "exec" || stdin == nil {
		return k.fakeKling.Run(ctx, stdin, args...)
	}
	in, _ := io.ReadAll(stdin)
	sql := string(in)
	k.scripts = append(k.scripts, sql)
	switch {
	case strings.Contains(sql, tcMarker+":discover"):
		return []byte(k.disc + "\n"), nil
	case !strings.Contains(sql, tcMarker+":run"):
		return nil, fmt.Errorf("tenantKling: unexpected stdin %q", sql)
	}
	n := len(k.tenants)
	if n > 5 {
		n = 5
	}
	total := k.rows * len(k.tenants)
	var out strings.Builder
	emit := func(format string, a ...any) { fmt.Fprintf(&out, format+"\n", a...) }
	for _, line := range strings.Split(sql, "\n") {
		switch {
		case strings.HasPrefix(line, "SELECT 'U '"):
			emit("U true")
		case line == `\echo T :kt_n`:
			emit("T %d", n)
		case line == `\echo DONE`:
			emit("DONE")
		case strings.HasPrefix(line, `\echo O `):
			f := strings.Fields(line)
			if tenantOK(f[2], n) {
				emit("O %s %s", f[2], f[3])
			}
		}
		m := ktMarker.FindStringSubmatch(line)
		if m == nil || !tenantOK(m[2], n) {
			continue
		}
		test, tag, j := m[1], m[2], m[3]
		none := noTenant(tag)
		switch {
		case test == tcTestSelect && none && k.failOpen:
			emit("S %s %s %d %d", tag, j, total, total)
		case test == tcTestSelect && none:
			emit("E %s %s 42704", tag, j)
		case test == tcTestSelect:
			emit("S %s %s %d 0", tag, j, k.rows)
		case none && !k.failOpen:
			emit("W %s %s %s 42704 0", test, tag, j)
		case none && test == tcTestInsert:
			emit("W %s %s %s 23505 0", test, tag, j) // pasó la política; lo paró la clave primaria
		case none && test == tcTestMove:
			emit("W %s %s %s 00000 1", test, tag, j)
		case none:
			emit("W %s %s %s 00000 %d", test, tag, j, total)
		case test == tcTestUpdate:
			emit("W %s %s %s 00000 0", test, tag, j)
		default:
			emit("W %s %s %s 42501 0", test, tag, j)
		}
	}
	return []byte(out.String()), nil
}

// tenantOK: el escenario existe (los de inquilino, solo hasta n).
func tenantOK(tag string, n int) bool {
	if noTenant(tag) {
		return true
	}
	var i int
	_, err := fmt.Sscanf(tag, "t%d", &i)
	return err == nil && i >= 1 && i <= n
}

const auraPolicy = `((current_setting('app.tenant_id'::text, true) IS NULL) OR (current_setting('app.tenant_id'::text, true) = ''::text) OR (tenant_id = (current_setting('app.tenant_id'::text, true))::uuid))`

const closedPolicy = `(tenant_id = (current_setting('app.tenant_id'::text))::uuid)`

func discJSON(policy string, extra ...tcDiscTable) string {
	d := tcDiscovery{RoleExists: true, Tables: []tcDiscTable{
		{Schema: "public", Table: "accounts", RLS: true, Sel: true, Ins: true, Upd: true,
			Cols: []string{"id", "tenant_id", "name"},
			Policies: []tcDiscPolicy{{Name: "tenant_isolation", Permissive: "PERMISSIVE", Cmd: "ALL",
				Roles: []string{"public"}, Qual: policy, Applies: true}}},
		{Schema: "public", Table: "contacts", RLS: true, Sel: true, Ins: true, Upd: true,
			Cols: []string{"id", "tenant_id", "email"},
			Policies: []tcDiscPolicy{{Name: "contacts_tenant", Permissive: "PERMISSIVE", Cmd: "ALL",
				Roles: []string{"public"}, Qual: policy, Applies: true}}},
	}}
	d.Tables = append(d.Tables, extra...)
	b, _ := json.Marshal(d)
	return string(b)
}

func newTenantApp(t *testing.T, failOpen bool, disc string) (*testApp, *tenantKling) {
	t.Helper()
	ta := newTestApp(t)
	ta.f.newMachine("crm", map[string]string{labelGolden: "pg", labelOwner: "local", labelState: stateReady})
	k := &tenantKling{fakeKling: ta.f, disc: disc, failOpen: failOpen, rows: 3,
		tenants: []string{"a1b2c3d4-0000-4000-8000-00000000aaaa", "a1b2c3d4-0000-4000-8000-00000000bbbb", "secret-tenant'); DROP TABLE x; --"}}
	ta.app.k = k
	return ta, k
}

var defaultTC = tcOpts{column: defaultTenantCol, setting: defaultTenantVar, max: defaultTenantMax}

// Un caso "tipo AuraCRM": la política deja pasar todo sin inquilino.
func TestTenantCheckFailOpenFalla(t *testing.T) {
	ta, k := newTenantApp(t, true, discJSON(auraPolicy))
	rep, err := ta.app.tenantCheck(context.Background(), "crm", "local", defaultTC)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pass || rep.Failed == 0 {
		t.Fatalf("fail-open policy passed: %+v", rep)
	}
	if rep.Tenants != 3 || rep.Role != "app" || rep.Database != "appdb" {
		t.Fatalf("report header: %+v", rep)
	}
	for _, tb := range rep.Tables {
		if tb.Status != tcStatusTableFail {
			t.Errorf("%s: status %s, want fail", tb.Table, tb.Status)
		}
		why := strings.Join(tb.Causes, "\n")
		if !strings.Contains(why, "fail-open in USING") || !strings.Contains(why, "IS NULL") {
			t.Errorf("%s: causes do not name the IS NULL branch: %q", tb.Table, why)
		}
		// Con inquilino fijado la política es correcta: los fallos son sin él.
		for _, c := range tb.Checks {
			if c.Result == tcFail && !noTenant(c.Scenario) {
				t.Errorf("%s: unexpected failure with a tenant set: %+v", tb.Table, c)
			}
		}
	}
	var out bytes.Buffer
	if err := writeTenantReport(&out, rep, false); err != nil {
		t.Fatal(err)
	}
	txt := out.String()
	for _, want := range []string{"FAIL   public.accounts", "SELECT sees 9 row(s) with no tenant set",
		`policy "tenant_isolation" (permissive, ALL): fail-open in USING`, "only a constraint stopped it (23505)",
		"fix:  make the policy fail closed", "summary: 2 table(s), 2 not passing"} {
		if !strings.Contains(txt, want) {
			t.Errorf("report lacks %q:\n%s", want, txt)
		}
	}
	// Los valores de inquilino no salen de la base: ni en la SQL que se manda
	// ni en el informe.
	for _, v := range k.tenants {
		for _, s := range append(k.scripts, txt) {
			if strings.Contains(s, v) {
				t.Fatalf("tenant value %q leaked", v)
			}
		}
	}
}

// Una política correcta (falla cerrada) pasa.
func TestTenantCheckCorrectaPasa(t *testing.T) {
	ta, _ := newTenantApp(t, false, discJSON(closedPolicy))
	rep, err := ta.app.tenantCheck(context.Background(), "crm", "local", defaultTC)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Pass || rep.Failed != 0 || rep.Errors != 0 {
		var out bytes.Buffer
		_ = writeTenantReport(&out, rep, false)
		t.Fatalf("correct policy did not pass:\n%s", out.String())
	}
	for _, tb := range rep.Tables {
		if len(tb.Causes) != 0 || len(tb.Policies[0].Problems) != 0 {
			t.Errorf("%s: causes %v, problems %v", tb.Table, tb.Causes, tb.Policies[0].Problems)
		}
		// null, empty y 3 inquilinos: SELECT + 3 escrituras cada uno.
		if len(tb.Checks) != 5*4 {
			t.Errorf("%s: %d checks, want 20: %+v", tb.Table, len(tb.Checks), tb.Checks)
		}
	}
	var out bytes.Buffer
	if err := writeTenantReport(&out, rep, true); err != nil {
		t.Fatal(err)
	}
	var back tcReport
	if err := json.Unmarshal(out.Bytes(), &back); err != nil || !back.Pass || len(back.Tables) != 2 {
		t.Fatalf("-json: %v %+v", err, back)
	}
}

// Los nombres de la base van citados; los raros no se prueban.
func TestTenantCheckNombres(t *testing.T) {
	weird := tcDiscTable{Schema: "public", Table: `we"ird`, RLS: true, Sel: true, Cols: []string{"tenant_id"}}
	ctrl := tcDiscTable{Schema: "public", Table: "bad\x1b[2Jname", RLS: true, Sel: true}
	ta, k := newTenantApp(t, false, discJSON(closedPolicy, weird, ctrl))
	rep, err := ta.app.tenantCheck(context.Background(), "crm", "local", defaultTC)
	if err != nil {
		t.Fatal(err)
	}
	run := k.scripts[len(k.scripts)-1]
	if !strings.Contains(run, `FROM "public"."we""ird"`) {
		t.Fatalf("weird name not quoted:\n%s", run)
	}
	if strings.Contains(run, "\x1b") {
		t.Fatal("a name with control characters reached the script")
	}
	var bad *tcTable
	for _, tb := range rep.Tables {
		if strings.HasPrefix(tb.Table, "bad") {
			bad = tb
		}
	}
	if bad == nil || bad.Status != tcError || rep.Pass {
		t.Fatalf("control-character table: %+v, pass=%v", bad, rep.Pass)
	}
	var out bytes.Buffer
	_ = writeTenantReport(&out, rep, false)
	if strings.Contains(out.String(), "\x1b") {
		t.Fatal("control characters printed")
	}
}

func TestTenantCheckRolSinRLS(t *testing.T) {
	d := discJSON(closedPolicy)
	d = strings.Replace(d, `"bypassrls":false`, `"bypassrls":true`, 1)
	ta, _ := newTenantApp(t, false, d)
	rep, err := ta.app.tenantCheck(context.Background(), "crm", "local", defaultTC)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pass || !strings.Contains(rep.RoleProblem, "BYPASSRLS") {
		t.Fatalf("BYPASSRLS role passed: %+v", rep)
	}
}

func TestTenantCheckErrores(t *testing.T) {
	for _, o := range []tcOpts{
		{column: "tenant id", setting: defaultTenantVar, max: 5},
		{column: defaultTenantCol, setting: "tenant", max: 5},
		{column: defaultTenantCol, setting: "app.tenant_id'--", max: 5},
		{column: defaultTenantCol, setting: tcOtherVar, max: 5},
		{column: defaultTenantCol, setting: defaultTenantVar, max: 0},
		{column: defaultTenantCol, setting: defaultTenantVar, max: maxTenantMax + 1},
		{role: "postgres", column: defaultTenantCol, setting: defaultTenantVar, max: 5},
	} {
		if err := o.validate(); err == nil {
			t.Errorf("%+v: accepted", o)
		}
	}

	// Sin tablas con la columna, sin el rol, copia ajena o parada.
	for name, c := range map[string]struct {
		disc, owner string
		mutate      func(*api.Machine)
		want        string
	}{
		"no tables": {disc: `{"role_exists":true,"tables":[]}`, owner: "local", want: "has a \"tenant_id\" column"},
		"no role":   {disc: `{"role_exists":false,"tables":[]}`, owner: "local", want: "does not exist"},
		"not owned": {disc: discJSON(closedPolicy), owner: "otro", want: "belongs to owner"},
		"frozen": {disc: discJSON(closedPolicy), owner: "local", want: "not running",
			mutate: func(mc *api.Machine) { mc.State = api.StateWarm }},
	} {
		ta, _ := newTenantApp(t, false, c.disc)
		if c.mutate != nil {
			c.mutate(ta.f.find("crm"))
		}
		_, err := ta.app.tenantCheck(context.Background(), "crm", c.owner, defaultTC)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", name, err, c.want)
		}
	}
}

// Sin la línea DONE (psql paró en la preparación) no hay informe.
func TestTenantCheckIncompleto(t *testing.T) {
	ta, k := newTenantApp(t, false, discJSON(closedPolicy))
	ta.app.k = &truncKling{k}
	if _, err := ta.app.tenantCheck(context.Background(), "crm", "local", defaultTC); err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("err %v", err)
	}
}

type truncKling struct{ *tenantKling }

func (k *truncKling) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	out, err := k.tenantKling.Run(ctx, stdin, args...)
	return bytes.ReplaceAll(out, []byte("DONE"), nil), err
}

func TestClassifyWrite(t *testing.T) {
	for _, c := range []struct {
		test, tag, code string
		rows            int64
		want            string
	}{
		{tcTestInsert, "t1", "42501", 0, tcPass},
		{tcTestInsert, "t1", "23505", 0, tcFail},
		{tcTestInsert, "t1", "00000", 1, tcFail},
		{tcTestInsert, "t1", "00000", 0, tcSkip},
		{tcTestInsert, tcScenarioNull, "00000", 0, tcPass},
		{tcTestMove, "t2", "22P02", 0, tcPass},
		{tcTestUpdate, "t2", "00000", 0, tcPass},
		{tcTestUpdate, tcScenarioEmpty, "00000", 4, tcFail},
		{tcTestUpdate, "t2", "55P03", 0, tcSkip},
	} {
		got := classifyWrite(c.test, c.tag, tcWriteRow{code: c.code, rows: c.rows}, true)
		if got.Result != c.want {
			t.Errorf("%s %s %s/%d: %s (%s), want %s", c.test, c.tag, c.code, c.rows, got.Result, got.Detail, c.want)
		}
	}
	if got := classifyWrite(tcTestInsert, "t1", tcWriteRow{}, false); got.Result != tcError {
		t.Errorf("missing result: %s", got.Result)
	}
}

func TestPolicyProblems(t *testing.T) {
	for _, c := range []struct {
		qual, want string
	}{
		{auraPolicy, "fail-open in USING"},
		{"true", "USING (true)"},
		{"(owner = CURRENT_USER)", "does not read app.tenant_id"},
		{closedPolicy, ""},
	} {
		got := strings.Join(policyProblems(tcDiscPolicy{Qual: c.qual}, true, defaultTenantVar), "|")
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%q: %q, want %q", c.qual, got, c.want)
		}
	}
}
