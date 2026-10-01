package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/ext/db/internal/doctor"
	"github.com/juan52878911/kindling/pkg/api"
)

// testAppMotores es un testApp con una plantilla Redis "rd" y una SQLite "sq"
// (las etiquetas que ponen db-golden-redis.sh y db-golden-sqlite.sh).
func testAppMotores(t *testing.T) *testApp {
	t.Helper()
	ta := newTestApp(t)
	ta.f.snaps["rd"] = &api.Snapshot{Name: "rd", Labels: map[string]string{
		api.LabelPorts: "8080", labelEngine: engineRedis, labelRole: "app"}}
	ta.f.snaps["sq"] = &api.Snapshot{Name: "sq", Labels: map[string]string{
		labelEngine: engineSQLite, labelDatabase: "appdb"}}
	return ta
}

// ── Redis ────────────────────────────────────────────────────────────────────

// up de una plantilla Redis: la copia nace con el motor y el 6379, espera al
// servidor, estrena la clave del administrador dentro del invitado y recibe
// solo el SHA-256 de la clave de la aplicación, que se queda en el host.
func TestUpRedisRotaConHash(t *testing.T) {
	ta := testAppMotores(t)
	mc, err := ta.up(ctx, "rd", "r1", 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	live := ta.f.machines[mc.ID]
	if live.Labels[labelEngine] != engineRedis || live.Labels[api.LabelPorts] != "6379,8080" || live.Labels[labelState] != stateReady {
		t.Fatalf("labels %v", live.Labels)
	}
	pw := password(t, mc.ID)
	if ta.f.verifier[mc.ID] != redisHash(pw) {
		t.Fatalf("el invitado guarda %q, no el hash de la clave del host", ta.f.verifier[mc.ID])
	}
	if ta.f.adminRot[mc.ID] != 1 {
		t.Fatalf("la clave del administrador se estrenó %d veces", ta.f.adminRot[mc.ID])
	}
	assertNoLeak(t, ta.f, pw)
	for _, c := range ta.f.calls {
		j := strings.Join(c.args, " ")
		if strings.Contains(j, "psql") || strings.Contains(j, "mysql") {
			t.Fatalf("una orden de otro motor: %v", c.args)
		}
		// La clave del administrador nunca sale del invitado: ni en argv ni
		// en lo que manda el host.
		if strings.Contains(c.stdin, "REDISCLI_AUTH=") && !strings.Contains(c.stdin, `REDISCLI_AUTH=$`) {
			t.Fatalf("una clave de administrador literal en stdin: %q", c.stdin)
		}
	}
	ta.printReady(mc)
	if !strings.Contains(ta.out.String(), "-redis") || strings.Contains(ta.out.String(), pw) {
		t.Fatalf("printReady: %q", ta.out.String())
	}
}

// Si el servidor no contesta o la rotación falla, la copia no se entrega.
func TestUpRedisFallidaDestruye(t *testing.T) {
	for name, set := range map[string]func(*fakeKling){
		"guion falla":      func(f *fakeKling) { f.failRotation = 1 },
		"hash distinto":    func(f *fakeKling) { f.wrongVerifier = true },
		"redis no arranca": func(f *fakeKling) { f.pgDown = true },
	} {
		t.Run(name, func(t *testing.T) {
			ta := testAppMotores(t)
			set(ta.f)
			if _, err := ta.up(ctx, "rd", "r1", 0, "local"); err == nil {
				t.Fatal("want error")
			}
			if len(ta.f.machines) != 0 || passwordFiles(t) != 0 {
				t.Fatalf("quedan máquinas o claves: %v", ta.f.machines)
			}
		})
	}
}

// fork de una copia Redis: cada copia estrena su clave (y la del
// administrador), distinta del origen y entre ellas.
func TestForkRedis(t *testing.T) {
	ta := testAppMotores(t)
	src, err := ta.up(ctx, "rd", "r1", 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	copies, err := ta.fork(ctx, "r1", 2, "local")
	if err != nil {
		t.Fatal(err)
	}
	vistos := map[string]bool{ta.f.verifier[src.ID]: true}
	for _, c := range copies {
		h := ta.f.verifier[c.ID]
		if vistos[h] || h != redisHash(password(t, c.ID)) || engineOf(c.Labels) != engineRedis {
			t.Fatalf("copia %s: hash %q repetido o no rotado", c.Name, h)
		}
		if ta.f.adminRot[c.ID] != 1 {
			t.Fatalf("copia %s: el administrador no estrenó clave", c.Name)
		}
		vistos[h] = true
	}
}

// connect: dirección, DSN redis:// y el redis-cli del host con la clave en el
// entorno; -psql, -mysql, -sqlite y -role no valen para una copia Redis.
func TestConnectRedis(t *testing.T) {
	ta := testAppMotores(t)
	mc, err := ta.up(ctx, "rd", "r1", 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	pw := password(t, mc.ID)
	if err := ta.connect(ctx, "r1", "local", "info"); err != nil {
		t.Fatal(err)
	}
	out := ta.out.String()
	if !strings.Contains(out, "port      6379") || !strings.Contains(out, "database  0") || !strings.Contains(out, "-redis") || strings.Contains(out, pw) {
		t.Fatalf("info: %q", out)
	}
	ta.out.Reset()
	if err := ta.connect(ctx, "r1", "local", "dsn"); err != nil {
		t.Fatal(err)
	}
	if want := "redis://app:" + pw + "@" + mc.IP + ":6379/0\n"; ta.out.String() != want {
		t.Fatalf("dsn %q, quería %q", ta.out.String(), want)
	}
	if err := ta.connect(ctx, "r1", "local", "redis"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(ta.redis, " ")
	if !strings.HasPrefix(got, "REDISCLI_AUTH="+pw+" ") || strings.Count(got, pw) != 1 || !strings.HasSuffix(got, "-p 6379 --user app") {
		t.Fatalf("cliente: %q", got)
	}
	for _, mode := range []string{"psql", "mysql", "sqlite"} {
		if err := ta.connect(ctx, "r1", "local", mode); err == nil || !strings.Contains(err.Error(), "-redis") {
			t.Errorf("-%s en una copia Redis: %v", mode, err)
		}
	}
	if err := ta.connectAs(ctx, "r1", "local", "info", "agent"); err == nil || !strings.Contains(err.Error(), "postgres copies only") {
		t.Fatalf("-role en una copia Redis: %v", err)
	}
}

// En macOS la copia se alcanza por el reenvío del 6379.
func TestConnectRedisMacOS(t *testing.T) {
	ta := testAppMotores(t)
	ta.f.macOS = true
	mc, err := ta.up(ctx, "rd", "r1", 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	h, port, err := hostAddr(ta.f.machines[mc.ID])
	if err != nil || h != "127.0.0.1" || port < 29000 {
		t.Fatalf("hostAddr %s:%d %v", h, port, err)
	}
}

// rotate: hash nuevo en la base y en el host, sin tocar al administrador; si
// falla, la vieja sigue en los dos sitios.
func TestRotateRedis(t *testing.T) {
	ta := testAppMotores(t)
	mc, err := ta.up(ctx, "rd", "r1", 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	vieja := password(t, mc.ID)
	if _, err := ta.rotateCopy(ctx, "r1", "local"); err != nil {
		t.Fatal(err)
	}
	nueva := password(t, mc.ID)
	if nueva == vieja || ta.f.verifier[mc.ID] != redisHash(nueva) || ta.f.adminRot[mc.ID] != 1 {
		t.Fatalf("rotación: hash %v, administrador %d", ta.f.verifier[mc.ID] == redisHash(nueva), ta.f.adminRot[mc.ID])
	}
	ta.f.failRotation = ta.f.rotations + 1
	if _, err := ta.rotateCopy(ctx, "r1", "local"); err == nil || !strings.Contains(err.Error(), "previous password is unchanged") {
		t.Fatalf("rotación fallida: %v", err)
	}
	if password(t, mc.ID) != nueva || ta.f.verifier[mc.ID] != redisHash(nueva) {
		t.Fatal("tras el fallo la clave vigente cambió")
	}
	assertNoLeak(t, ta.f, nueva)
}

// El guion de rotación lleva el hash y no la clave, y rechaza lo que no sea
// un usuario simple o un hash de 64 hexadecimales.
func TestRedisRotateScript(t *testing.T) {
	h := redisHash("secreto")
	s := redisRotateScript("app", h, true)
	if strings.Contains(s, "secreto") || !strings.Contains(s, "\nA="+h+"\n") || !strings.Contains(s, "ACL SAVE") || !strings.Contains(s, "ACL SETUSER default resetpass") {
		t.Fatalf("guion:\n%s", s)
	}
	if s := redisRotateScript("app", h, false); strings.Contains(s, "SETUSER default") {
		t.Fatal("sin admin no se toca al administrador")
	}
	ta := testAppMotores(t)
	for _, c := range [][2]string{{"default", h}, {"a'b", h}, {"app", "abc"}, {"app", strings.ToUpper(h)}} {
		if err := ta.setRedisHash(ctx, "x", c[0], c[1], false); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Errorf("%q %q: %v", c[0], c[1], err)
		}
	}
	if len(ta.f.calls) != 0 {
		t.Fatal("se llamó a kling con datos inválidos")
	}
}

// redisHolds: activo, con exactamente esa clave; vale el orden de Redis 7.
func TestRedisHolds(t *testing.T) {
	h, otro := redisHash("a"), redisHash("b")
	for _, c := range []struct {
		out  string
		want bool
	}{
		{"flags\non\nsanitize-payload\npasswords\n" + h + "\ncommands\n+@all -@admin\n", true},
		{"flags\noff\npasswords\n" + h + "\n", false},
		{"flags\non\npasswords\n" + h + "\n" + otro + "\n", false},
		{"flags\non\nnopass\npasswords\n", false},
		{"flags\non\npasswords\n" + otro + "\n", false},
		{"ERR something\n", false},
	} {
		if got := redisHolds(c.out, h); got != c.want {
			t.Errorf("%q: %v, want %v", c.out, got, c.want)
		}
	}
}

// ── SQLite ───────────────────────────────────────────────────────────────────

// up de una plantilla SQLite: sin puerto ni contraseña; se comprueba que la
// base se abre y se marca lista.
func TestUpSQLite(t *testing.T) {
	ta := testAppMotores(t)
	mc, err := ta.up(ctx, "sq", "s1", 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	live := ta.f.machines[mc.ID]
	if live.Labels[labelEngine] != engineSQLite || live.Labels[api.LabelPorts] != "" || live.Labels[labelState] != stateReady {
		t.Fatalf("labels %v", live.Labels)
	}
	if ta.f.sqliteOpens[mc.ID] != 1 || ta.f.rotations != 0 {
		t.Fatalf("aperturas %d, rotaciones %d", ta.f.sqliteOpens[mc.ID], ta.f.rotations)
	}
	if _, err := dbstate.ReadPassword(mc.ID); err == nil {
		t.Fatal("una copia SQLite no tiene contraseña")
	}
	for _, c := range ta.f.calls {
		if j := strings.Join(c.args, " "); strings.Contains(j, "sqlite3") && !strings.Contains(j, "-readonly") {
			t.Fatalf("la comprobación no es de solo lectura: %v", c.args)
		}
	}
	ta.printReady(mc)
	if !strings.Contains(ta.out.String(), "-sqlite") || !strings.Contains(ta.out.String(), "/var/lib/kling-db/appdb.sqlite") {
		t.Fatalf("printReady: %q", ta.out.String())
	}

	ta2 := testAppMotores(t)
	ta2.f.pgDown = true
	if _, err := ta2.up(ctx, "sq", "s1", 0, "local"); err == nil || len(ta2.f.machines) != 0 {
		t.Fatalf("una base que no abre: %v, quedan %v", err, ta2.f.machines)
	}
}

// connect: sin dirección ni DSN; -sqlite abre sqlite3 dentro con kling shell.
func TestConnectSQLite(t *testing.T) {
	ta := testAppMotores(t)
	mc, err := ta.up(ctx, "sq", "s1", 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	if err := ta.connect(ctx, "s1", "local", "info"); err != nil {
		t.Fatal(err)
	}
	if out := ta.out.String(); !strings.Contains(out, "/var/lib/kling-db/appdb.sqlite") || !strings.Contains(out, "no password") || strings.Contains(out, "port") {
		t.Fatalf("info: %q", out)
	}
	if err := ta.connect(ctx, "s1", "local", "sqlite"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ta.kling, " "); got != "shell "+mc.ID+" -- sqlite3 /var/lib/kling-db/appdb.sqlite" {
		t.Fatalf("kling %q", got)
	}
	for _, mode := range []string{"dsn", "psql", "mysql", "redis"} {
		if err := ta.connect(ctx, "s1", "local", mode); err == nil || !strings.Contains(err.Error(), "-sqlite") {
			t.Errorf("%s en una copia SQLite: %v", mode, err)
		}
	}
	if _, err := ta.rotateCopy(ctx, "s1", "local"); err == nil || !strings.Contains(err.Error(), "copies only") {
		t.Fatalf("rotate en una copia SQLite: %v", err)
	}
	// Una copia que no está lista no se entrega, aunque no tenga clave.
	ta.f.machines[mc.ID].Labels[labelState] = statePreparing
	if err := ta.connect(ctx, "s1", "local", "info"); err == nil {
		t.Fatal("connect a una copia en preparación")
	}
}

// fork, reset y rm valen igual para una copia SQLite.
func TestForkResetRmSQLite(t *testing.T) {
	ta := testAppMotores(t)
	src, err := ta.up(ctx, "sq", "s1", 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	copies, err := ta.fork(ctx, "s1", 2, "local")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range copies {
		if c.Labels[labelState] != stateReady || engineOf(c.Labels) != engineSQLite || ta.f.sqliteOpens[c.ID] != 1 {
			t.Fatalf("copia %s: %v", c.Name, c.Labels)
		}
	}
	nueva, err := ta.reset(ctx, "s1", "local")
	if err != nil {
		t.Fatal(err)
	}
	if nueva.ID == src.ID || nueva.Name != "s1" || ta.f.machines[src.ID] != nil {
		t.Fatalf("reset: %+v", nueva)
	}
	if err := ta.remove(ctx, ta.f.machines[nueva.ID]); err != nil || ta.f.machines[nueva.ID] != nil {
		t.Fatalf("rm: %v", err)
	}
}

// ── lo que no hacen ──────────────────────────────────────────────────────────

// Lo que esta versión no hace con Redis ni SQLite se rechaza antes de tocar
// nada.
func TestRedisSQLiteRechazan(t *testing.T) {
	migs := t.TempDir()
	if err := os.WriteFile(filepath.Join(migs, "001.sql"), []byte("SELECT 1;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tpl := range []string{"rd", "sq"} {
		t.Run(tpl, func(t *testing.T) {
			ta := testAppMotores(t)
			mc, err := ta.up(ctx, tpl, "c1", 0, "local")
			if err != nil {
				t.Fatal(err)
			}
			otra, err := ta.up(ctx, tpl, "c2", 0, "local")
			if err != nil {
				t.Fatal(err)
			}
			ag, _ := ta.up(ctx, "pg", "agente", 0, "local")
			ta.f.machines[ag.ID].Egress = "allowlist"
			ta.f.snaps["app-tpl"] = &api.Snapshot{Name: "app-tpl"}
			antes := len(ta.f.calls)
			errs := map[string]error{}
			_, errs["snapshot"] = ta.snapshot(ctx, "c1", "s1", "local", false)
			_, errs["undo"] = ta.undo(ctx, "c1", "", "local")
			errs["attach"] = ta.attach(ctx, "agente", "c1", attachOpts{owner: "local", env: "PGPASSWORD"})
			_, _, _, errs["role"] = ta.roleTarget(ctx, "c1", "local", "agent")
			_, errs["tenant-check"] = ta.tenantCheck(ctx, "c1", "local", tcOpts{column: "tenant_id", setting: "app.tenant_id", max: 5})
			_, errs["ask"] = ta.askPrepare(ctx, "c1", "local", "")
			_, errs["diff"] = ta.diff(ctx, "c1", "c2", "local", diffOpts{maxRows: 10})
			_, errs["rehearse copy"] = ta.rehearse(ctx, "c1", "local", migs, nil, time.Second, false)
			_, errs["rehearse template"] = ta.rehearse(ctx, tpl, "local", migs, nil, time.Second, false)
			o := envOptsT("tienda")
			o.golden = tpl
			_, errs["env"] = ta.envUp(ctx, o)
			errs["audit"] = requireEngine(mc, "audit", enginePostgres, engineMySQL)
			errs["branch"] = ta.writeBranchEnv(otra, filepath.Join(t.TempDir(), "env"))
			for cmd, err := range errs {
				if err == nil || !strings.Contains(err.Error(), "only") || !strings.Contains(err.Error(), "docs/db-engines.md") {
					t.Errorf("%s en una copia %s: %v", cmd, tpl, err)
				}
			}
			for _, c := range ta.f.calls[antes:] {
				if len(c.args) > 0 && (c.args[0] == "exec" || c.args[0] == "save" || c.args[0] == "sandbox" || c.args[0] == "run" || c.args[0] == "graph") || c.labels != nil {
					t.Fatalf("se tocó algo: %v %v", c.args, c.labels)
				}
			}
		})
	}
}

// goldenInfo conoce los cuatro motores y rechaza los demás (mongodb no está).
func TestGoldenInfoMotores(t *testing.T) {
	t.Setenv("KLING_DB_STATE", t.TempDir())
	for _, e := range []string{engineRedis, engineSQLite, engineMySQL, enginePostgres} {
		if _, _, got, err := goldenInfo(&api.Snapshot{Name: "g", Labels: map[string]string{labelEngine: e}}); err != nil || got != e {
			t.Errorf("%s: %s %v", e, got, err)
		}
	}
	if _, _, _, err := goldenInfo(&api.Snapshot{Name: "g", Labels: map[string]string{labelEngine: "mongodb"}}); err == nil {
		t.Error("mongodb no es un motor de kling db")
	}
	if _, _, _, err := goldenInfo(&api.Snapshot{Name: "g", Labels: map[string]string{labelEngine: engineRedis, labelRole: "default"}}); err == nil {
		t.Error("default es el administrador de Redis, no el usuario de la aplicación")
	}
	for _, c := range []struct {
		e    string
		port int
	}{{enginePostgres, 5432}, {engineMySQL, 3306}, {engineRedis, 6379}, {engineSQLite, 0}} {
		if got := enginePort(c.e); got != c.port {
			t.Errorf("enginePort(%s) = %d", c.e, got)
		}
	}
}

// El doctor entra en una copia Redis con la misma orden que kling db.
func TestRedisAdminIgualQueDoctor(t *testing.T) {
	if redisAdmin != doctor.RedisAdminClient {
		t.Fatalf("kling db y el doctor usan clientes distintos:\n%s\n%s", redisAdmin, doctor.RedisAdminClient)
	}
}
