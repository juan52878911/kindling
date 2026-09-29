package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/ext/db/internal/mysqlpw"
	"github.com/juan52878911/kindling/pkg/api"
)

// testAppMy es un testApp con una plantilla MySQL "my" (la etiqueta que pone
// db-golden-mysql.sh).
func testAppMy(t *testing.T) *testApp {
	t.Helper()
	ta := newTestApp(t)
	ta.f.snaps["my"] = &api.Snapshot{Name: "my", Labels: map[string]string{
		api.LabelPorts: "8080", labelEngine: engineMySQL, labelRole: "app", labelDatabase: "appdb"}}
	return ta
}

// up de una plantilla MySQL: la copia nace con el motor y el 3306 en sus
// etiquetas, espera al servidor, recibe solo el HASH de su clave nueva (que se
// queda en el host) y se marca lista después de rotar.
func TestUpMySQLRotaConHash(t *testing.T) {
	ta := testAppMy(t)
	mc, err := ta.up(ctx, "my", "m1", 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	live := ta.f.machines[mc.ID]
	if live.Labels[labelEngine] != engineMySQL || live.Labels[api.LabelPorts] != "3306,8080" || live.Labels[labelState] != stateReady {
		t.Fatalf("labels %v", live.Labels)
	}
	pw := password(t, mc.ID)
	if ta.f.verifier[mc.ID] != mysqlpw.NativeHash(pw) {
		t.Fatalf("el invitado guarda %q, no el hash de la clave del host", ta.f.verifier[mc.ID])
	}
	assertNoLeak(t, ta.f, pw)
	var ping, alter, psql bool
	for _, c := range ta.f.calls {
		j := strings.Join(c.args, " ")
		ping = ping || strings.Contains(j, "mysqladmin")
		alter = alter || strings.Contains(c.stdin, "ALTER USER 'app'@'%' IDENTIFIED WITH mysql_native_password AS '*")
		psql = psql || strings.Contains(j, "psql") || strings.Contains(j, "pg_isready")
	}
	if !ping || !alter || psql {
		t.Fatalf("ping %v, alter %v, psql %v", ping, alter, psql)
	}
	ta.printReady(mc)
	if !strings.Contains(ta.out.String(), "-mysql") || strings.Contains(ta.out.String(), pw) {
		t.Fatalf("printReady: %q", ta.out.String())
	}
}

// Si el servidor no arranca o la rotación falla, la copia no se entrega.
func TestUpMySQLFallidaDestruye(t *testing.T) {
	for name, set := range map[string]func(*fakeKling){
		"cliente falla":    func(f *fakeKling) { f.failRotation = 1 },
		"hash distinto":    func(f *fakeKling) { f.wrongVerifier = true },
		"mysql no arranca": func(f *fakeKling) { f.pgDown = true },
	} {
		t.Run(name, func(t *testing.T) {
			ta := testAppMy(t)
			set(ta.f)
			if _, err := ta.up(ctx, "my", "m1", 0, "local"); err == nil {
				t.Fatal("want error")
			} else if strings.Contains(err.Error(), "ALTER USER") {
				t.Fatalf("el error cita la sentencia: %v", err)
			}
			if len(ta.f.machines) != 0 {
				t.Fatalf("quedan máquinas: %v", ta.f.machines)
			}
		})
	}
}

// fork de una copia MySQL: cada copia estrena su clave (hash distinto del
// origen y entre ellas).
func TestForkMySQL(t *testing.T) {
	ta := testAppMy(t)
	src, err := ta.up(ctx, "my", "m1", 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	copies, err := ta.fork(ctx, "m1", 2, "local")
	if err != nil {
		t.Fatal(err)
	}
	vistos := map[string]bool{ta.f.verifier[src.ID]: true}
	for _, c := range copies {
		h := ta.f.verifier[c.ID]
		if vistos[h] || h != mysqlpw.NativeHash(password(t, c.ID)) || engineOf(c.Labels) != engineMySQL {
			t.Fatalf("copia %s: hash %q repetido o no rotado", c.Name, h)
		}
		vistos[h] = true
	}
}

// connect: dirección, DSN mysql:// y el cliente del host con la clave en el
// entorno; -psql no vale para una copia MySQL (ni -mysql para una Postgres).
func TestConnectMySQL(t *testing.T) {
	ta := testAppMy(t)
	mc, err := ta.up(ctx, "my", "m1", 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	pw := password(t, mc.ID)
	if err := ta.connect(ctx, "m1", "local", "info"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ta.out.String(), "port      3306") || strings.Contains(ta.out.String(), pw) {
		t.Fatalf("info: %q", ta.out.String())
	}
	ta.out.Reset()
	if err := ta.connect(ctx, "m1", "local", "dsn"); err != nil {
		t.Fatal(err)
	}
	want := "mysql://app:" + pw + "@" + mc.IP + ":3306/appdb\n"
	if ta.out.String() != want {
		t.Fatalf("dsn %q, quería %q", ta.out.String(), want)
	}
	if err := ta.connect(ctx, "m1", "local", "mysql"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(ta.mysql, " ")
	if !strings.HasPrefix(got, "MYSQL_PWD="+pw+" ") || strings.Count(got, pw) != 1 || !strings.Contains(got, "-P 3306 -u app appdb") {
		t.Fatalf("cliente: %q", got)
	}
	if err := ta.connect(ctx, "m1", "local", "psql"); err == nil || !strings.Contains(err.Error(), "-mysql") {
		t.Fatalf("-psql en una copia MySQL: %v", err)
	}
	if err := ta.connectAs(ctx, "m1", "local", "info", "agent"); err == nil {
		t.Fatal("-role en una copia MySQL debería rechazarse")
	}
	pg, err := ta.up(ctx, "pg", "p1", 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	if err := ta.connect(ctx, pg.Name, "local", "mysql"); err == nil || !strings.Contains(err.Error(), "-psql") {
		t.Fatalf("-mysql en una copia Postgres: %v", err)
	}
}

// En macOS la copia se alcanza por el reenvío del 3306.
func TestConnectMySQLmacOS(t *testing.T) {
	ta := testAppMy(t)
	ta.f.macOS = true
	mc, err := ta.up(ctx, "my", "m1", 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	h, port, err := hostAddr(ta.f.machines[mc.ID])
	if err != nil || h != "127.0.0.1" || port < 29000 {
		t.Fatalf("hostAddr %s:%d %v", h, port, err)
	}
}

// rotate: hash nuevo en la base y en el host; si falla, la vieja sigue en los
// dos sitios.
func TestRotateMySQL(t *testing.T) {
	ta := testAppMy(t)
	mc, err := ta.up(ctx, "my", "m1", 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	vieja := password(t, mc.ID)
	if _, err := ta.rotateCopy(ctx, "m1", "local"); err != nil {
		t.Fatal(err)
	}
	nueva := password(t, mc.ID)
	if nueva == vieja || ta.f.verifier[mc.ID] != mysqlpw.NativeHash(nueva) {
		t.Fatal("la rotación no cambió la clave o el hash")
	}
	ta.f.failRotation = ta.f.rotations + 1
	if _, err := ta.rotateCopy(ctx, "m1", "local"); err == nil || !strings.Contains(err.Error(), "previous password is unchanged") {
		t.Fatalf("rotación fallida: %v", err)
	}
	if password(t, mc.ID) != nueva || ta.f.verifier[mc.ID] != mysqlpw.NativeHash(nueva) {
		t.Fatal("tras el fallo la clave vigente cambió")
	}
	p, _ := dbstate.PasswordPath(mc.ID)
	if _, err := os.Stat(filepath.Join(filepath.Dir(p), "password.new")); err == nil {
		t.Fatal("quedó la clave a medias")
	}
}

// Lo que esta versión no hace con MySQL se rechaza antes de tocar nada.
func TestMySQLSoloPostgres(t *testing.T) {
	ta := testAppMy(t)
	mc, err := ta.up(ctx, "my", "m1", 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	ag, _ := ta.up(ctx, "pg", "agente", 0, "local")
	ta.f.machines[ag.ID].Egress = "allowlist"
	antes := len(ta.f.calls)
	errs := map[string]error{}
	_, errs["snapshot"] = ta.snapshot(ctx, "m1", "s1", "local", false)
	_, errs["undo"] = ta.undo(ctx, "m1", "", "local")
	errs["attach"] = ta.attach(ctx, "agente", "m1", attachOpts{owner: "local", env: "PGPASSWORD"})
	_, _, _, errs["role"] = ta.roleTarget(ctx, "m1", "local", "agent")
	_, errs["tenant-check"] = ta.tenantCheck(ctx, "m1", "local", tcOpts{column: "tenant_id", setting: "app.tenant_id", max: 5})
	for cmd, err := range errs {
		if err == nil || !strings.Contains(err.Error(), "postgres copies only") {
			t.Errorf("%s en una copia MySQL: %v", cmd, err)
		}
	}
	for _, c := range ta.f.calls[antes:] {
		if len(c.args) > 0 && (c.args[0] == "exec" || c.args[0] == "save" || c.args[0] == "sandbox") || c.labels != nil {
			t.Fatalf("se tocó la copia: %v %v", c.args, c.labels)
		}
	}
	_ = mc
}

// El cliente del host: la opción para apagar TLS depende de cuál sea, y
// "mysql" puede ser el de MariaDB. Si --version falla, no se cae en
// --ssl-mode=DISABLED (que MariaDB rechaza): se mira --help.
func TestMySQLClientTLSFlag(t *testing.T) {
	const helpMariaDB = "Usage: mysql [OPTIONS] [database]\n  --ssl  Enable SSL for connection (automatically enabled with other flags). (Defaults to on; use --skip-ssl to disable.)\n"
	const helpMySQL = "mysql  Ver 8.4.2 for Linux on x86_64 (MySQL Community Server - GPL)\n  --ssl-mode=name  SSL connection mode.\n"
	falla := errors.New("exit status 1")
	casos := []struct {
		nombre, bin     string
		version, help   string
		verErr, helpErr error
		want            string
		error           bool
	}{
		{"binario mariadb", "/usr/bin/mariadb", "", "", falla, falla, "--skip-ssl", false},
		{"mysql de MariaDB", "/usr/bin/mysql", "mysql from 11.4.2-MariaDB, client 15.2 for Linux", "", nil, falla, "--skip-ssl", false},
		{"mysql de MariaDB 10", "/usr/bin/mysql", "mysql  Ver 15.1 Distrib 10.11.6-MariaDB, for debian-linux-gnu", "", nil, nil, "--skip-ssl", false},
		{"mysql de verdad", "/usr/bin/mysql", "mysql  Ver 8.4.2 for Linux on x86_64 (MySQL Community Server - GPL)", helpMySQL, nil, nil, "--ssl-mode=DISABLED", false},
		{"--version falla, --help de MariaDB", "/usr/bin/mysql", "", helpMariaDB, falla, nil, "--skip-ssl", false},
		{"--version falla, --help de MySQL", "/usr/bin/mysql", "", helpMySQL, falla, nil, "--ssl-mode=DISABLED", false},
		{"nada lo aclara", "/usr/bin/mysql", "", "", falla, falla, "", true},
		{"--help sin opciones de TLS", "/usr/bin/mysql", "mysql 1.0", "Usage: mysql", nil, nil, "", true},
	}
	for _, c := range casos {
		probe := func(_ context.Context, p, arg string) (string, error) {
			if p != c.bin {
				t.Fatalf("%s: probed %s", c.nombre, p)
			}
			if arg == "--version" {
				return c.version, c.verErr
			}
			return c.help, c.helpErr
		}
		got, err := mysqlClientTLSFlag(context.Background(), c.bin, probe)
		if (err != nil) != c.error || got != c.want {
			t.Errorf("%s: %q %v, want %q (error %v)", c.nombre, got, err, c.want, c.error)
		}
		if err != nil && !strings.Contains(err.Error(), "-dsn") {
			t.Errorf("%s: the error should point to -dsn: %v", c.nombre, err)
		}
	}
}
