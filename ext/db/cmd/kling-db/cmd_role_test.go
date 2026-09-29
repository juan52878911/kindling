package main

import (
	"context"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
)

// roleKling envuelve al falso del daemon y contesta al psql y al sh -s que
// usa `kling db role`: una tabla de roles, las líneas de pg_hba.conf y el
// registro de todo lo que llegó por stdin.
type roleKling struct {
	*fakeKling
	roles   map[string]*fakeRole // nombre -> rol (una sola copia basta)
	hba     map[string]bool
	stdins  []string
	schemas string
	// failCreate hace fallar el CREATE ROLE; wrongAttrs contesta "f" a la
	// comprobación; badHBA dice que pg_hba no tiene la regla válida.
	failCreate, wrongAttrs, badHBA bool
	events                         []string
}

type fakeRole struct{ comment, verifier string }

var (
	createRoleRe = regexp.MustCompile(`CREATE ROLE "([^"]+)"`)
	hbaLineRe    = regexp.MustCompile(`L='host all "([^"]+)" `)
)

func newRoleKling(f *fakeKling) *roleKling {
	return &roleKling{fakeKling: f, roles: map[string]*fakeRole{}, hba: map[string]bool{}, schemas: "public\nsales\n"}
}

func (r *roleKling) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	if len(args) == 0 || args[0] != "exec" || stdin == nil {
		return r.fakeKling.Run(ctx, stdin, args...)
	}
	in, _ := io.ReadAll(stdin)
	sql := string(in)
	if strings.Contains(sql, "ALTER ROLE app PASSWORD") {
		return r.fakeKling.Run(ctx, strings.NewReader(sql), args...)
	}
	r.stdins = append(r.stdins, sql)
	r.fakeKling.mu.Lock()
	r.fakeKling.calls = append(r.fakeKling.calls, call{args: append([]string(nil), args...), stdin: sql})
	r.fakeKling.mu.Unlock()
	cmd := strings.Join(args, " ")
	switch {
	case strings.HasSuffix(cmd, "sh -s"):
		m := hbaLineRe.FindStringSubmatch(sql)
		if m == nil {
			return nil, io.ErrUnexpectedEOF
		}
		add := strings.Contains(sql, `>> "$T"`)
		r.hba[m[1]] = add
		if !add {
			delete(r.hba, m[1])
		}
		r.events = append(r.events, map[bool]string{true: "hba+", false: "hba-"}[add]+m[1])
		return nil, nil
	case strings.Contains(sql, "shobj_description"):
		name := between(sql, "rolname = '", "'")
		if ro, ok := r.roles[name]; ok {
			return []byte("1:" + ro.comment + "\n"), nil
		}
		return []byte("0:\n"), nil
	case strings.Contains(sql, "FROM pg_namespace"):
		return []byte(r.schemas), nil
	case strings.Contains(sql, "CREATE ROLE"):
		if r.failCreate {
			return nil, io.ErrClosedPipe
		}
		name := createRoleRe.FindStringSubmatch(sql)[1]
		r.roles[name] = &fakeRole{comment: roleComment, verifier: between(sql, "PASSWORD '", "'")}
		r.events = append(r.events, "create "+name)
		if r.wrongAttrs {
			return []byte("f\n"), nil
		}
		return []byte("t\n"), nil
	case strings.Contains(sql, "DROP ROLE"):
		name := between(sql, "DROP ROLE \"", "\"")
		delete(r.roles, name)
		r.events = append(r.events, "drop "+name)
		return nil, nil
	case strings.Contains(sql, "pg_hba_file_rules"):
		if r.badHBA {
			return []byte("t\n0\n"), nil
		}
		return []byte("t\n1\n"), nil
	case strings.Contains(sql, "pg_reload_conf"):
		return []byte("t\n"), nil
	}
	return nil, io.ErrUnexpectedEOF
}

func between(s, a, b string) string {
	_, rest, ok := strings.Cut(s, a)
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(rest, b)
	return v
}

func roleApp(t *testing.T) (*testApp, *roleKling, string) {
	t.Helper()
	ta := newTestApp(t)
	rk := newRoleKling(ta.f)
	ta.app.k = rk
	mc := readyCopy(t, ta, "c1")
	return ta, rk, mc.ID
}

func rolePW(t *testing.T, id, role string) string {
	t.Helper()
	pw, err := dbstate.ReadRolePassword(id, role)
	if err != nil {
		t.Fatalf("password of role %s: %v", role, err)
	}
	return pw
}

func TestRoleROCrea(t *testing.T) {
	ta, rk, id := roleApp(t)
	if err := ta.roleCreateRO(ctx, "c1", "local", "agent", nil, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	pw := rolePW(t, id, "agent")
	assertVerifierMatches(t, rk.roles["agent"].verifier, pw)
	if !rk.hba["agent"] {
		t.Fatal("no pg_hba line for the role")
	}
	p, _ := dbstate.RolePasswordPath(id, "agent")
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("password file %v %v", st, err)
	}
	// La contraseña de la aplicación sigue en su sitio y es otra.
	if password(t, id) == pw {
		t.Fatal("the role reuses the password of the application")
	}

	var create string
	for _, s := range rk.stdins {
		if strings.Contains(s, "CREATE ROLE") {
			create = s
		}
	}
	for _, want := range []string{
		`CREATE ROLE "agent" LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE`,
		"CONNECTION LIMIT 5",
		`ALTER ROLE "agent" SET default_transaction_read_only = on;`,
		`ALTER ROLE "agent" SET statement_timeout = '5000ms';`,
		`ALTER ROLE "agent" SET idle_in_transaction_session_timeout = '5000ms';`,
		`GRANT USAGE ON SCHEMA "public" TO "agent";`,
		`GRANT SELECT ON ALL TABLES IN SCHEMA "sales" TO "agent";`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE "app" IN SCHEMA "public" GRANT SELECT ON TABLES TO "agent";`,
		`\connect appdb`,
	} {
		if !strings.Contains(create, want) {
			t.Errorf("missing %q in\n%s", want, create)
		}
	}
	// Ni un privilegio de escritura ni una pertenencia.
	for _, bad := range []string{"INSERT", "UPDATE", "DELETE", "ALL PRIVILEGES", "GRANT pg_", "SUPERUSER;", "CREATE ON"} {
		if strings.Contains(create, bad) {
			t.Errorf("the role script has %q", bad)
		}
	}
	// El rol se crea y comprueba antes de abrirle la red.
	if len(rk.events) < 2 || rk.events[0] != "create agent" || rk.events[1] != "hba+agent" {
		t.Fatalf("events %v", rk.events)
	}
	assertNoLeak(t, ta.f, pw)
	if strings.Contains(ta.out.String(), pw) || strings.Contains(ta.err.String(), pw) {
		t.Fatal("password on the output")
	}
	for _, s := range rk.stdins {
		if strings.Contains(s, pw) {
			t.Fatal("the plain password went into the guest")
		}
	}
}

func TestRoleROEsquemas(t *testing.T) {
	ta, rk, id := roleApp(t)
	if err := ta.roleCreateRO(ctx, "c1", "local", "agent", []string{"sales", " sales"}, time.Second); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(rk.stdins, "\n")
	if strings.Contains(all, `SCHEMA "public" TO "agent"`) || !strings.Contains(all, `SCHEMA "sales" TO "agent"`) {
		t.Fatalf("schemas not honoured:\n%s", all)
	}
	if !strings.Contains(all, "statement_timeout = '1000ms'") {
		t.Fatal("timeout not honoured")
	}
	_ = id

	// Un esquema que no existe, o con un nombre raro: error antes de crear nada.
	for _, bad := range [][]string{{"nope"}, {`x"; DROP`}, {""}, {"Sales"}} {
		ta2, rk2, id2 := roleApp2(t)
		if err := ta2.roleCreateRO(ctx, "c2", "local", "agent", bad, time.Second); err == nil {
			t.Fatalf("schemas %q: want error", bad)
		}
		if len(rk2.roles) != 0 || len(rk2.hba) != 0 {
			t.Fatalf("schemas %q: something was created", bad)
		}
		if _, err := dbstate.ReadRolePassword(id2, "agent"); err == nil {
			t.Fatalf("schemas %q: password written", bad)
		}
	}
}

func roleApp2(t *testing.T) (*testApp, *roleKling, string) {
	t.Helper()
	ta := newTestApp(t)
	rk := newRoleKling(ta.f)
	ta.app.k = rk
	mc, err := ta.up(ctx, "pg", "c2", 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	return ta, rk, mc.ID
}

func TestRoleNombres(t *testing.T) {
	ta, rk, _ := roleApp(t)
	for _, name := range []string{
		"postgres", "app", "", "Agent", "a-b", "1abc", "pg_monitor", "pg_x", "all", "public", "none",
		"replication", "user", strings.Repeat("a", 64), `a"b`, "a b", "añ", "a;b",
	} {
		if err := ta.roleCreateRO(ctx, "c1", "local", name, nil, time.Second); err == nil {
			t.Errorf("name %q: want error", name)
		}
		if err := ta.roleRemove(ctx, "c1", "local", name); err == nil {
			t.Errorf("rm name %q: want error", name)
		}
	}
	if len(rk.roles) != 0 || len(rk.hba) != 0 {
		t.Fatal("a bad name created something")
	}
	for _, name := range []string{"agent", "_x", "a1_b2", strings.Repeat("a", 63)} {
		if err := validRoleName(name, "app"); err != nil {
			t.Errorf("name %q: %v", name, err)
		}
	}
}

func TestRoleTimeout(t *testing.T) {
	ta, _, _ := roleApp(t)
	for _, d := range []time.Duration{0, -time.Second, time.Millisecond, 2 * time.Hour} {
		if err := ta.roleCreateRO(ctx, "c1", "local", "agent", nil, d); err == nil {
			t.Errorf("timeout %v: want error", d)
		}
	}
}

func TestRoleROFallaYDeshace(t *testing.T) {
	for name, set := range map[string]func(*roleKling){
		"atributos":    func(r *roleKling) { r.wrongAttrs = true },
		"pg_hba":       func(r *roleKling) { r.badHBA = true },
		"create falla": func(r *roleKling) { r.failCreate = true },
	} {
		t.Run(name, func(t *testing.T) {
			ta, rk, id := roleApp(t)
			set(rk)
			err := ta.roleCreateRO(ctx, "c1", "local", "agent", nil, time.Second)
			if err == nil {
				t.Fatal("want error")
			}
			if len(rk.roles) != 0 || len(rk.hba) != 0 {
				t.Fatalf("not undone: roles %v hba %v", rk.roles, rk.hba)
			}
			if _, err := dbstate.ReadRolePassword(id, "agent"); err == nil {
				t.Fatal("password left on the host")
			}
			if strings.Contains(err.Error(), "SCRAM") {
				t.Fatalf("the error quotes the statement: %v", err)
			}
			// La copia y su contraseña siguen bien.
			password(t, id)
		})
	}
}

func TestRoleROYaExiste(t *testing.T) {
	ta, rk, id := roleApp(t)
	if err := ta.roleCreateRO(ctx, "c1", "local", "agent", nil, time.Second); err != nil {
		t.Fatal(err)
	}
	pw := rolePW(t, id, "agent")
	ver := rk.roles["agent"].verifier
	if err := ta.roleCreateRO(ctx, "c1", "local", "agent", nil, time.Second); err == nil {
		t.Fatal("want error: exists")
	}
	if rolePW(t, id, "agent") != pw || rk.roles["agent"].verifier != ver {
		t.Fatal("the existing role or its password were touched")
	}
}

func TestRoleRm(t *testing.T) {
	ta, rk, id := roleApp(t)
	if err := ta.roleCreateRO(ctx, "c1", "local", "agent", nil, time.Second); err != nil {
		t.Fatal(err)
	}
	ta.out.Reset()
	if err := ta.roleRemove(ctx, "c1", "local", "agent"); err != nil {
		t.Fatal(err)
	}
	if len(rk.roles) != 0 || len(rk.hba) != 0 {
		t.Fatalf("roles %v hba %v", rk.roles, rk.hba)
	}
	if _, err := dbstate.ReadRolePassword(id, "agent"); err == nil {
		t.Fatal("password left")
	}
	// Sesiones fuera y permisos fuera antes de soltar el rol.
	all := strings.Join(rk.stdins, "\n")
	for _, want := range []string{"pg_terminate_backend", `DROP OWNED BY "agent"`, `DROP ROLE "agent"`} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Idempotente: sin rol limpia lo que quedara en el host.
	if err := dbstate.WriteRolePassword(id, "agent", "left"); err != nil {
		t.Fatal(err)
	}
	if err := ta.roleRemove(ctx, "c1", "local", "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := dbstate.ReadRolePassword(id, "agent"); err == nil {
		t.Fatal("leftover password not cleaned")
	}
}

func TestRoleRmNoTocaRolesAjenos(t *testing.T) {
	ta, rk, id := roleApp(t)
	rk.roles["reporting"] = &fakeRole{comment: ""} // creado a mano, no por kling db
	if err := ta.roleRemove(ctx, "c1", "local", "reporting"); err == nil {
		t.Fatal("want error")
	}
	if rk.roles["reporting"] == nil {
		t.Fatal("dropped a role that kling db did not create")
	}
	_ = id
}

func TestRoleRechazaCopiaAjenaONoLista(t *testing.T) {
	ta, rk, id := roleApp(t)
	if err := ta.roleCreateRO(ctx, "c1", "otro", "agent", nil, time.Second); err == nil {
		t.Fatal("want error: another owner")
	}
	ta.f.machines[id].Labels[labelState] = statePreparing
	if err := ta.roleCreateRO(ctx, "c1", "local", "agent", nil, time.Second); err == nil {
		t.Fatal("want error: not ready")
	}
	ta.f.machines[id].Labels[labelState] = stateReady
	ta.f.machines[id].State = "frozen"
	if err := ta.roleCreateRO(ctx, "c1", "local", "agent", nil, time.Second); err == nil {
		t.Fatal("want error: not running")
	}
	if err := ta.roleCreateRO(ctx, "nope", "local", "agent", nil, time.Second); err == nil {
		t.Fatal("want error: no machine")
	}
	if len(rk.roles) != 0 {
		t.Fatal("created a role in a copy that is not usable")
	}
}

func TestConnectConRol(t *testing.T) {
	ta, _, id := roleApp(t)
	if err := ta.roleCreateRO(ctx, "c1", "local", "agent", nil, time.Second); err != nil {
		t.Fatal(err)
	}
	pw, appPW := rolePW(t, id, "agent"), password(t, id)

	ta.out.Reset()
	if err := ta.connectAs(ctx, "c1", "local", "dsn", "agent"); err != nil {
		t.Fatal(err)
	}
	want := "postgres://agent:" + pw + "@" + ta.f.machines[id].IP + ":5432/appdb?sslmode=disable\n"
	if ta.out.String() != want {
		t.Fatalf("dsn %q, want %q", ta.out.String(), want)
	}
	ta.out.Reset()
	if err := ta.connectAs(ctx, "c1", "local", "psql", "agent"); err != nil {
		t.Fatal(err)
	}
	if ta.psql[0] != "PGPASSWORD="+pw || !strings.Contains(strings.Join(ta.psql, " "), "-U agent") {
		t.Fatalf("psql %v", ta.psql)
	}
	if strings.Contains(strings.Join(ta.psql[1:], " "), pw) || strings.Contains(strings.Join(ta.psql, " "), appPW) {
		t.Fatal("wrong or leaked password")
	}
	ta.out.Reset()
	if err := ta.connectAs(ctx, "c1", "local", "info", "agent"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ta.out.String(), pw) || !strings.Contains(ta.out.String(), "agent.password") ||
		!strings.Contains(ta.out.String(), "-role agent") {
		t.Fatalf("info %q", ta.out.String())
	}

	// Sin rol o con un nombre inválido, no entrega nada; el rol de la app no
	// se pide por -role.
	for _, r := range []string{"ghost", "app", "postgres", "Bad", "../x"} {
		ta.out.Reset()
		if err := ta.connectAs(ctx, "c1", "local", "dsn", r); err == nil || ta.out.Len() != 0 {
			t.Errorf("role %q: %v %q", r, err, ta.out.String())
		}
	}
	// Sigue siendo una copia lista y propia.
	if err := ta.connectAs(ctx, "c1", "otro", "dsn", "agent"); err == nil {
		t.Fatal("want error: another owner")
	}
	// Los ficheros de rol siguen las mismas exigencias que el de la app.
	p, _ := dbstate.RolePasswordPath(id, "agent")
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ta.connectAs(ctx, "c1", "local", "dsn", "agent"); err == nil {
		t.Fatal("want error: password file readable by others")
	}
}

func TestRmDeLaCopiaBorraLasClavesDeRoles(t *testing.T) {
	ta, _, id := roleApp(t)
	if err := ta.roleCreateRO(ctx, "c1", "local", "agent", nil, time.Second); err != nil {
		t.Fatal(err)
	}
	mc, _ := ta.inspect(ctx, "c1")
	if err := ta.remove(ctx, mc); err != nil {
		t.Fatal(err)
	}
	if _, err := dbstate.ReadRolePassword(id, "agent"); err == nil {
		t.Fatal("role password survived kling db rm")
	}
}
