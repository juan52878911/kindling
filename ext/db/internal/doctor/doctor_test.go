package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Verificadores calculados aparte (Python, hashlib.pbkdf2_hmac) para estas
// contraseñas; el primero es el vector de RFC 7677 (usuario "user", "pencil").
const (
	vecPencil = "SCRAM-SHA-256$4096:W22ZaJ0SNY7soEsUEjb6gQ==$WG5d8oPm3OtcPnkdi4Uo7BkeZkBFzpcXkuLmtbsT4qY=:wfPLwcE6nTWhTAmQ7tl2KeoiWGPlZqQxSrmfPwDl2dU="

	goldenPW  = "0123456789abcdef0123456789abcdef0123456789abcdef"
	goldenVer = "SCRAM-SHA-256$4096:c2FsdGRlbGRvcmFkbzE2$9+DaSF3NjG34Fe4xs8PYjAp/aF0lCbkNXHRPs0FvP50=:hVM5SbnXcaZaY1f2AOx80uCCSxleKK/Up3s8wXs7chY="
	copyPW    = "fedcba9876543210fedcba9876543210fedcba9876543210"
	copyVer   = "SCRAM-SHA-256$4096:c2FsZGVsYWNvcGlhMTY=$+JzNm+60cGI3q3mDJqVcAA7gh8XVgUYuVjJ3xJxq/ds=:VyWCger38vy82mEey0Gz8fG1ywojPiR0xPKJCMDDSdY="
)

func TestVerificadorRFC7677(t *testing.T) {
	v, err := parseVerifier(vecPencil)
	if err != nil {
		t.Fatal(err)
	}
	if !v.matches("pencil") || v.matches("pencil2") {
		t.Fatal("el vector de RFC 7677 no cuadra")
	}
	for _, bad := range []string{"md5abc", "SCRAM-SHA-256$0:c2Fs$AA==:AA==", "SCRAM-SHA-256$99999999:c2Fs$x:y", "SCRAM-SHA-256$4096:c2Fs$AA==:AA=="} {
		if _, err := parseVerifier(bad); err == nil {
			t.Errorf("aceptó %q", bad)
		}
	}
}

// kFalso es un kling que contesta con salidas de psql grabadas. La consulta se
// reconoce por su marca /* doctor:<nombre> */ y la base por el -d de psql.
type kFalso struct {
	machine api.Machine
	sql     map[string]string // "<db>/<nombre>" o "<nombre>"
	skew    time.Duration     // desvío del reloj del invitado

	mu    sync.Mutex
	stdin []string
}

var reMarca = regexp.MustCompile(`^/\* doctor:([a-z0-9_]+) \*/`)

func (f *kFalso) Run(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	switch {
	case len(args) == 2 && args[0] == "inspect":
		if args[1] != f.machine.Name && args[1] != f.machine.ID {
			return nil, errors.New("no such machine")
		}
		return json.Marshal(f.machine)
	case len(args) > 3 && args[0] == "exec" && args[len(args)-2] == "date":
		return []byte(strconv.FormatInt(time.Now().Add(f.skew).Unix(), 10) + "\n"), nil
	case len(args) > 3 && args[0] == "exec" && strings.HasPrefix(args[len(args)-1], "psql "):
		if args[2] != f.machine.ID {
			return nil, fmt.Errorf("exec on %q, want the id", args[2])
		}
		if stdin == nil {
			return nil, errors.New("psql without stdin")
		}
		b, _ := io.ReadAll(stdin)
		f.mu.Lock()
		f.stdin = append(f.stdin, string(b))
		f.mu.Unlock()
		db := args[len(args)-1][strings.LastIndex(args[len(args)-1], "-d ")+3:]
		m := reMarca.FindStringSubmatch(string(b))
		if m == nil {
			return nil, errors.New("sql without marker")
		}
		if m[1] == "clock" {
			return fmt.Appendf(nil, "%.6f\n", float64(time.Now().Add(f.skew).UnixNano())/1e9), nil
		}
		if out, ok := f.sql[db+"/"+m[1]]; ok {
			return []byte(out + "\n"), nil
		}
		if out, ok := f.sql[m[1]]; ok {
			return []byte(out + "\n"), nil
		}
		if m[1] == "policies" || m[1] == "tenant_tables" || m[1] == "setrole" || m[1] == "clients" || m[1] == "md5" {
			return []byte("[]\n"), nil
		}
		return nil, fmt.Errorf("psql: no recorded output for %s/%s", db, m[1])
	}
	return nil, fmt.Errorf("unexpected kling %q", args)
}

func estado(t *testing.T, files map[string]string) {
	t.Helper()
	d := t.TempDir()
	for p, c := range files {
		full := filepath.Join(d, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("KLING_DB_STATE", d)
}

func maquina(labels map[string]string) api.Machine {
	return api.Machine{
		ID: "m-1a2b3c", Name: "copia", State: api.StateRunning,
		CreatedAt: time.Now().Add(-time.Minute), Labels: labels,
	}
}

func correr(t *testing.T, k *kFalso) (int, string) {
	t.Helper()
	var out bytes.Buffer
	n, err := Run(context.Background(), k, Target{Machine: "copia"}, &out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Log("\n" + out.String())
	return n, out.String()
}

func tiene(t *testing.T, out, sev, rule string) {
	t.Helper()
	if !regexp.MustCompile(`(?m)^  ` + sev + ` +` + rule + `  `).MatchString(out) {
		t.Errorf("falta %s %s en el informe", sev, rule)
	}
}

func noTiene(t *testing.T, out, rule string) {
	t.Helper()
	if regexp.MustCompile(`(?m)^  [A-Z]+ +` + rule + `  `).MatchString(out) {
		t.Errorf("sobra %s en el informe", rule)
	}
}

// Un caso "tipo AuraCRM": el rol de la app es superusuario con BYPASSRLS, las
// políticas dejan pasar todo si la variable de tenant es NULL o vacía, y la copia
// conserva la contraseña del dorado.
func TestCasoAuraCRM(t *testing.T) {
	estado(t, map[string]string{
		"crm-golden/password": goldenPW,
		"crm-golden/conn.env": "PGUSER=crm_user\nPGDATABASE=auracrm\n",
	})
	k := &kFalso{
		machine: maquina(map[string]string{LabelGolden: "crm-golden", LabelState: StateReady}),
		sql: map[string]string{
			"databases": `["auracrm", "postgres"]`,
			"settings":  `{"ssl": "off", "user": "postgres", "password_encryption": "md5"}`,
			"roles": `[{"name": "crm_user", "super": true, "bypassrls": true, "createrole": true, "dangerous": []},
			           {"name": "postgres", "super": true, "bypassrls": true, "createrole": true, "dangerous": []},
			           {"name": "reporting", "super": false, "bypassrls": false, "createrole": false, "dangerous": ["pg_execute_server_program"]}]`,
			"md5": `["crm_user"]`,
			"auracrm/policies": `[{"schema": "public", "tbl": "contacts", "name": "tenant_isolation", "permissive": "PERMISSIVE", "cmd": "ALL",
			  "qual": "((current_setting('app.current_tenant'::text, true) IS NULL) OR (current_setting('app.current_tenant'::text, true) = ''::text) OR (tenant_id = (current_setting('app.current_tenant'::text, true))::uuid))",
			  "with_check": ""}]`,
			"auracrm/tenant_tables": `[{"schema": "public", "tbl": "contacts", "rls": true, "force": false, "owner": "crm_user"},
			                          {"schema": "public", "tbl": "leads", "rls": false, "force": false, "owner": "crm_user"}]`,
			"app_verifier": `{"exists": true, "verifier": "` + goldenVer + `"}`,
			"clients":      fmt.Sprintf(`[{"pid": 77, "user": "crm_user", "start": %d}]`, time.Now().Add(-48*time.Hour).Unix()),
		},
	}
	n, out := correr(t, k)
	if n == 0 {
		t.Fatal("el caso AuraCRM no dio problemas")
	}
	tiene(t, out, "CRITICAL", "DB001") // crm_user superusuario
	tiene(t, out, "CRITICAL", "DB002") // BYPASSRLS
	tiene(t, out, "CRITICAL", "DB004") // reporting en pg_execute_server_program
	tiene(t, out, "CRITICAL", "DB010") // política fail-open
	tiene(t, out, "HIGH", "DB011")     // leads sin RLS
	tiene(t, out, "HIGH", "DB012")     // contacts sin FORCE y del rol de la app
	tiene(t, out, "WARN", "DB030")
	tiene(t, out, "WARN", "DB031")
	tiene(t, out, "CRITICAL", "DB052") // contraseña del dorado
	tiene(t, out, "WARN", "DB051")     // cliente heredado
	tiene(t, out, "HIGH", "DB054")     // lista pero sin fichero de contraseña
	tiene(t, out, "INFO", "DB021")
	if strings.Contains(out, goldenPW) || strings.Contains(out, goldenVer) {
		t.Fatal("el informe enseña la contraseña o el verificador")
	}
	if !strings.Contains(out, "summary:") {
		t.Fatal("sin resumen")
	}
	// El rol de la app sale del conn.env del dorado, validado.
	for _, s := range k.stdin {
		if strings.Contains(s, "doctor:app_verifier") && !strings.Contains(s, "rolname = 'crm_user'") {
			t.Fatalf("verifier query for the wrong role: %s", s)
		}
	}
}

func limpio() (*kFalso, map[string]string) {
	k := &kFalso{
		machine: maquina(map[string]string{LabelGolden: "pg-golden", LabelState: StateReady, LabelOwner: "juan"}),
		sql: map[string]string{
			"databases": `["appdb", "postgres"]`,
			"settings":  `{"ssl": "off", "user": "postgres", "password_encryption": "scram-sha-256"}`,
			"roles": `[{"name": "app", "super": false, "bypassrls": false, "createrole": false, "dangerous": []},
			           {"name": "postgres", "super": true, "bypassrls": true, "createrole": true, "dangerous": []}]`,
			"md5": `[]`,
			"appdb/policies": `[{"schema": "public", "tbl": "notes", "name": "by_tenant", "permissive": "PERMISSIVE", "cmd": "ALL",
			  "qual": "(tenant_id = (current_setting('app.tenant_id'::text))::uuid)",
			  "with_check": "(tenant_id = (current_setting('app.tenant_id'::text))::uuid)"}]`,
			"appdb/tenant_tables": `[{"schema": "public", "tbl": "notes", "rls": true, "force": true, "owner": "app"}]`,
			"app_verifier":        `{"exists": true, "verifier": "` + copyVer + `"}`,
			"clients":             fmt.Sprintf(`[{"pid": 90, "user": "app", "start": %d}]`, time.Now().Unix()),
		},
	}
	files := map[string]string{
		"pg-golden/password": goldenPW,
		"pg-golden/conn.env": "PGUSER=app\nPGDATABASE=appdb\n",
		"m-1a2b3c/password":  copyPW,
	}
	return k, files
}

func TestCasoLimpio(t *testing.T) {
	k, files := limpio()
	estado(t, files)
	n, out := correr(t, k)
	if n != 0 {
		t.Fatalf("problemas = %d, quería 0", n)
	}
	tiene(t, out, "INFO", "DB001") // postgres de arranque, solo informativo
	tiene(t, out, "INFO", "DB021")
	noTiene(t, out, "DB052")
	noTiene(t, out, "DB050")
	noTiene(t, out, "DB051") // la conexión del agente es posterior a la copia
}

func TestRotacionConVerificadorDelDorado(t *testing.T) {
	k, files := limpio()
	delete(files, "pg-golden/password")
	files["pg-golden/verifier"] = copyVer + "\n" // la copia NO rotó
	estado(t, files)
	_, out := correr(t, k)
	tiene(t, out, "CRITICAL", "DB052")
}

func TestRotacionNoVerificable(t *testing.T) {
	k, files := limpio()
	delete(files, "pg-golden/password")
	estado(t, files)
	n, out := correr(t, k)
	tiene(t, out, "WARN", "DB052")
	if n != 1 {
		t.Fatalf("problemas = %d, quería 1", n)
	}
}

func TestFicheroDeLaCopia(t *testing.T) {
	k, files := limpio()
	files["m-1a2b3c/password"] = goldenPW // no es la clave del verificador
	estado(t, files)
	_, out := correr(t, k)
	tiene(t, out, "HIGH", "DB054")

	k, files = limpio()
	estado(t, files)
	d := os.Getenv("KLING_DB_STATE")
	if err := os.Chmod(filepath.Join(d, "m-1a2b3c", "password"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, out = correr(t, k)
	tiene(t, out, "HIGH", "DB054")
}

func TestCopiaEnPreparacion(t *testing.T) {
	k, files := limpio()
	delete(files, "m-1a2b3c/password")
	k.machine.Labels[LabelState] = StatePreparing
	estado(t, files)
	_, out := correr(t, k)
	tiene(t, out, "HIGH", "DB053")
	tiene(t, out, "INFO", "DB054")
}

func TestRelojDesviado(t *testing.T) {
	k, files := limpio()
	k.skew = 30 * time.Second
	estado(t, files)
	_, out := correr(t, k)
	tiene(t, out, "HIGH", "DB050")
}

func TestMaquinaQueNoEsCopia(t *testing.T) {
	k, files := limpio()
	k.machine.Labels = nil
	estado(t, files)
	n, out := correr(t, k)
	tiene(t, out, "INFO", "DB059")
	noTiene(t, out, "DB053")
	if n != 0 {
		t.Fatalf("problemas = %d", n)
	}
}

func TestRolDeConnEnvHostil(t *testing.T) {
	k, files := limpio()
	files["pg-golden/conn.env"] = "PGUSER=app'; DROP ROLE x; --\n"
	estado(t, files)
	if _, err := Run(context.Background(), k, Target{Machine: "copia"}, io.Discard); err == nil {
		t.Fatal("aceptó un rol que no es un identificador")
	}
}

func TestTargetExactamenteUno(t *testing.T) {
	for _, tg := range []Target{{}, {Machine: "a", URL: "postgres://u@h/d"}} {
		if _, err := Run(context.Background(), &kFalso{}, tg, io.Discard); err == nil {
			t.Errorf("%+v: sin error", tg)
		}
	}
}

func TestFailOpen(t *testing.T) {
	casos := []struct {
		expr string
		bad  bool
	}{
		// deparseadas por pg_policies
		{"((current_setting('app.tenant_id'::text, true) IS NULL) OR (tenant_id = (current_setting('app.tenant_id'::text, true))::uuid))", true},
		{"((current_setting('app.tenant_id'::text, true) = ''::text) OR (tenant_id = (current_setting('app.tenant_id'::text, true))::uuid))", true},
		{"(NULLIF(current_setting('app.tenant'::text, true), ''::text) IS NULL)", true},
		{"(tenant_id = COALESCE((NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid, tenant_id))", true},
		{"COALESCE((tenant_id = (current_setting('app.tenant_id'::text, true))::uuid), true)", true},
		{"((''::text = current_setting('app.t'::text, true)) OR (tenant_id = 1))", true},
		{"((current_setting('app.t'::text, true))::character varying IS NULL)", true},
		// escritas a mano
		{"current_setting('app.tenant', true) is null or tenant = current_setting('app.tenant', true)", true},
		{"Current_Setting('app.tenant', true) = '' OR tenant = current_setting('app.tenant', true)", true},
		// fail-closed
		{"(tenant_id = (current_setting('app.tenant_id'::text))::uuid)", false},
		{"((current_setting('app.tenant_id'::text, true) IS NOT NULL) AND (tenant_id = (current_setting('app.tenant_id'::text, true))::uuid))", false},
		{"(tenant_id = COALESCE((current_setting('app.tenant_id'::text, true))::uuid, '00000000-0000-0000-0000-000000000000'::uuid))", false},
		{"CASE WHEN (current_setting('app.t'::text, true) IS NULL) THEN false ELSE (tenant_id = (current_setting('app.t'::text, true))::integer) END", false},
		{"(owner = CURRENT_USER)", false},
		{"", false},
		{"((current_setting('app.t'::text, true) <> ''::text) AND (tenant = current_setting('app.t'::text, true)))", false},
		{"(note = 'current_setting(x) IS NULL')", false},
	}
	for _, c := range casos {
		why, bad := failOpen(c.expr)
		if bad != c.bad {
			t.Errorf("failOpen(%q) = %v (%s), quería %v", c.expr, bad, why, c.bad)
		}
	}
}

// Un querier falso para el modo -url: mismas consultas, sin kling.
type qFalso map[string]string

func (f qFalso) query(_ context.Context, _, name, _ string) (string, error) {
	if out, ok := f[name]; ok {
		return out, nil
	}
	return "", errors.New("permission denied for table pg_authid")
}

func TestModoURLRemoto(t *testing.T) {
	r := &report{target: "x"}
	env := &sqlEnv{remote: true, who: "current_user", dbs: []string{"appdb"}}
	sqlChecks(context.Background(), qFalso{
		"settings": `{"ssl": "off", "user": "app", "password_encryption": "scram-sha-256"}`,
		"roles":    `[{"name": "app", "super": false, "bypassrls": false, "createrole": false, "dangerous": []}, {"name": "postgres", "super": true, "bypassrls": true, "createrole": true, "dangerous": []}]`,
		"setrole":  `[{"name": "admin", "super": true, "me_super": false}]`,
		"policies": `[]`, "tenant_tables": `[{"schema": "public", "tbl": "t", "rls": true, "force": false, "owner": "app"}]`,
	}, env, r)
	var out bytes.Buffer
	r.write(&out)
	t.Log("\n" + out.String())
	s := out.String()
	tiene(t, s, "HIGH", "DB040")     // ssl off en remoto
	tiene(t, s, "CRITICAL", "DB001") // postgres fuera de una copia sí cuenta
	tiene(t, s, "HIGH", "DB020")     // SET ROLE admin
	tiene(t, s, "HIGH", "DB012")     // dueño = current_user
	tiene(t, s, "INFO", "DB031")     // sin permiso para pg_authid
}

func TestParseURL(t *testing.T) {
	if _, err := parseURL("postgres://app:secreto@db.example:5432/appdb"); err == nil || strings.Contains(err.Error(), "secreto") {
		t.Fatalf("contraseña en la URL: %v", err)
	}
	if _, err := parseURL("postgres://app@db.example/appdb?sslmode=verify-full"); err == nil {
		t.Fatal("aceptó un sslmode que exige TLS")
	}
	if _, err := parseURL("mysql://app@h/d"); err == nil {
		t.Fatal("aceptó otro esquema")
	}
	p, err := parseURL("postgresql://app@127.0.0.1:29001")
	if err != nil || !p.loopback || p.db != "app" || p.addr != "127.0.0.1:29001" {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = parseURL("postgres://app@[::1]/appdb")
	if err != nil || !p.loopback || p.addr != "[::1]:5432" {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = parseURL("postgres://app@10.0.0.5/appdb")
	if err != nil || p.loopback {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestSafeQuitaEscapes(t *testing.T) {
	if s := safe("a\x1b[2Jb‮c\nd", 100); s != "a?[2Jb?c d" {
		t.Fatalf("safe = %q", s)
	}
	if s := safe("abcdef", 3); s != "abc..." {
		t.Fatalf("safe = %q", s)
	}
}
