package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Sin -step ni -sql los argumentos llegan intactos a db-golden.sh; con ellos,
// se separan, el orden de pasos y SQL se conserva y el script construye
// <nombre>-base.
func TestParseSteps(t *testing.T) {
	d := t.TempDir()
	sql := filepath.Join(d, "rls.sql")
	if err := os.WriteFile(sql, []byte("select 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := []string{"build", "-from", "pg16-ext", "-extension", "timescaledb", "-migrations", d, "aura"}
	rest, _, ok, err := parseSteps(in)
	if err != nil || ok || !slices.Equal(rest, in) {
		t.Fatalf("sin pasos: %v %v %v", rest, ok, err)
	}

	in = []string{"build", "-from", "pg16-ext", "-step", "alembic upgrade head", "-agent", "py", "-workdir", d,
		"-sql", sql, "-step", "python seed.py", "-keep", "-role", "crm_user", "aura", "-step-timeout", "5m"}
	rest, o, ok, err := parseSteps(in)
	if err != nil || !ok {
		t.Fatalf("con pasos: %v %v", ok, err)
	}
	want := []string{"build", "-from", "pg16-ext", "-keep", "-role", "crm_user", "aura-base"}
	if !slices.Equal(rest, want) {
		t.Errorf("al script: %v, quería %v", rest, want)
	}
	if o.name != "aura" || o.agent != "py" || o.workdir != d || !o.keep || o.timeout != 5*time.Minute {
		t.Errorf("opciones: %+v", o)
	}
	if len(o.steps) != 3 || o.steps[0].cmd != "alembic upgrade head" || o.steps[1].sql != sql || o.steps[2].cmd != "python seed.py" {
		t.Errorf("pasos: %+v", o.steps)
	}

	for _, bad := range [][]string{
		{"build", "-step", "x", "aura"},                                  // sin -agent
		{"build", "-agent", "py", "aura"},                                // -agent sin -step
		{"build", "-sql", filepath.Join(d, "no"), "aura"},                // no existe
		{"build", "-step", "x", "-agent", "py"},                          // sin nombre
		{"build", "-step", "x", "-agent", "py", "a", "b"},                // dos nombres
		{"build", "-step", "x", "-agent", "../py", "aura"},               // agente raro
		{"build", "-step", "x", "-agent", "py", "-workdir", sql, "aura"}, // no es dir
	} {
		if _, _, _, err := parseSteps(bad); err == nil {
			t.Errorf("aceptó %v", bad)
		}
	}
}

// -env-file puede llevar claves: 0600, y solo KEY=VALUE. El error no cita la
// línea (podría ser una clave).
func TestCheckEnvFile(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "env")
	if err := os.WriteFile(p, []byte("# x\nAPP_DB_PASSWORD=s3cr3t\n\nJWT=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkEnvFile(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkEnvFile(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("0644: %v", err)
	}
	if err := os.WriteFile(p, []byte("export s3cr3t-raro\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkEnvFile(p); err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("línea mala: %v", err)
	}
}

// El workdir sube sin .git ni node_modules ni enlaces.
func TestTarDir(t *testing.T) {
	d := t.TempDir()
	for _, f := range []string{"a/alembic.ini", "a/versions/001.py", ".git/HEAD", "node_modules/x/i.js", "b/__pycache__/c.pyc"} {
		p := filepath.Join(d, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(d, "a", "fuera")); err != nil {
		t.Fatal(err)
	}
	tgz, n, err := tarDir(d)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tgz)
	if n != 2 {
		t.Errorf("ficheros = %d, quería 2", n)
	}
	f, _ := os.Open(tgz)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	for _, n := range names {
		if strings.Contains(n, ".git") || strings.Contains(n, "node_modules") || strings.Contains(n, "fuera") || strings.Contains(n, "__pycache__") {
			t.Errorf("subió %s", n)
		}
	}
}

// El script de cada paso: entorno de -env-file, DATABASE_URL con el marcador y
// el comando como $1 (sin reinterpretar comillas).
func TestStepScript(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sin sh")
	}
	d := t.TempDir()
	script := strings.ReplaceAll(stepScript, "/work", d)
	script = strings.ReplaceAll(script, "/run/kdb-step.env", filepath.Join(d, "env"))
	if err := os.WriteFile(filepath.Join(d, "env"), []byte("EXTRA=valor con espacios\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("sh", "-c", script, "kdb-step", `echo "$DATABASE_URL|$EXTRA|$PGSSLMODE|$(pwd)"`)
	c.Env = append(os.Environ(), "PGHOST=copia.db.internal", "PGPORT=5432", "PGUSER=crm_user", "PGDATABASE=crm_db", "PGPASSWORD=kling-cred-x")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	want := "postgresql://crm_user:kling-cred-x@copia.db.internal:5432/crm_db?sslmode=disable|valor con espacios|disable|"
	if got := strings.TrimSpace(string(out)); !strings.HasPrefix(got, want) {
		t.Errorf("salida %q", got)
	}
}

func TestStrongLock(t *testing.T) {
	for norm, fuerte := range map[string]bool{
		"ALTER TABLE prospects ADD COLUMN x int":                                true,
		"alter table a add constraint f foreign key (b) references c (d)":       true,
		"ALTER TABLE a ADD CONSTRAINT f FOREIGN KEY (b) REFERENCES c NOT VALID": true,
		"CREATE INDEX ix ON a (b)":                                              true,
		"CREATE INDEX CONCURRENTLY ix ON a (b)":                                 false,
		"CREATE TABLE a (b int)":                                                false,
		"SELECT * FROM a":                                                       false,
		"DROP TABLE a":                                                          true,
		"REFRESH MATERIALIZED VIEW CONCURRENTLY v":                              false,
		"CREATE TRIGGER t BEFORE INSERT ON a":                                   true,
	} {
		l := strongLock(norm)
		// FOREIGN KEY ... NOT VALID sigue siendo ALTER TABLE: lock fuerte, pero
		// sin validar filas.
		if (l != "") != fuerte {
			t.Errorf("%q: %q", norm, l)
		}
	}
}

func TestSplitStatements(t *testing.T) {
	sql := "CREATE TABLE a (b text DEFAULT 'x;y'); -- c; d\n/* e; f */ ALTER TABLE a ADD c int;\n" +
		"CREATE FUNCTION f() RETURNS int AS $$ SELECT 1; $$ LANGUAGE sql;\nCREATE INDEX i ON a (b)"
	got := splitStatements(sql)
	if len(got) != 4 || !strings.HasPrefix(got[1], "ALTER TABLE") || !strings.Contains(got[2], "SELECT 1;") {
		t.Fatalf("%q", got)
	}
	st := strongStatements(sql)
	if len(st) != 2 || strings.Contains(strings.Join(st, " "), "x;y") {
		t.Fatalf("%q", st)
	}
}
