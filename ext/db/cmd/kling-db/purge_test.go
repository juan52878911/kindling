package main

// Una copia nueva no hereda los roles de `kling db role`: prepare los quita
// (con su línea de pg_hba.conf) antes de darla por lista, venga de up, fork o
// undo.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
)

// assertPurgedBeforeReady: en la copia id se purgaron los roles (SQL) y
// pg_hba.conf, en un solo exec (purgeScript), ANTES de marcarla ready, y ya
// no le queda ningún rol.
func assertPurgedBeforeReady(t *testing.T, f *fakeKling, id string) {
	t.Helper()
	purgeAt, readyAt, purges := -1, -1, 0
	for i, c := range f.calls {
		switch {
		case c.ref == id && c.labels[labelState] == stateReady:
			readyAt = i
		case len(c.args) > 0 && c.args[0] == "exec" && indexOf(c.args, id) >= 0 && strings.Contains(c.stdin, purgeMarker):
			if !strings.HasSuffix(strings.Join(c.args, " "), "sh -s") {
				t.Fatalf("copy %s: the purge is not one sh -s: %v", shortID(id), c.args)
			}
			purgeAt = i
			purges++
		}
	}
	if purgeAt < 0 || readyAt < 0 || purgeAt > readyAt || purges != 1 {
		t.Fatalf("copy %s: %d purge(s), the last @%d, ready@%d", shortID(id), purges, purgeAt, readyAt)
	}
	if len(f.roRoles[id]) != 0 {
		t.Fatalf("copy %s still has the roles %v", shortID(id), f.roRoles[id])
	}
}

func TestUpQuitaRolesHeredadosDeLaPlantilla(t *testing.T) {
	ta := newTestApp(t)
	// Un golden hecho de una copia que tenía un rol de solo lectura.
	ta.f.snapRoles["pg"] = []string{"agent"}
	mc, err := ta.up(ctx, "pg", "c1", 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	assertPurgedBeforeReady(t, ta.f, mc.ID)

	// Lo que se manda: el SQL quita los marcados kling-db:ro en la base de la
	// copia y en postgres, y comprueba que no queda ninguno; el sh saca las
	// líneas de pg_hba.conf del fichero que dice Postgres, y recarga.
	var sql string
	for _, c := range ta.f.calls {
		if strings.Contains(c.stdin, purgeMarker) {
			sql = c.stdin
		}
	}
	sh := sql // un solo guion: el SQL va en él como here-document
	for _, want := range []string{
		"pg_terminate_backend", `\connect appdb`, `\connect postgres`,
		"format('DROP OWNED BY %I', rolname)", "format('DROP ROLE %I', rolname)",
		"shobj_description(oid, 'pg_authid') = 'kling-db:ro'",
		"rolname = 'kling_db_ro' AND rolpassword IS NOT NULL",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("purge SQL lacks %q:\n%s", want, sql)
		}
	}
	if strings.Index(sql, `\connect appdb`) > strings.Index(sql, "DROP ROLE") {
		t.Error("DROP ROLE before DROP OWNED BY in the application database")
	}
	for _, want := range []string{"SHOW hba_file", "grep -v ' # kling-db$'", "pg_reload_conf"} {
		if !strings.Contains(sh, want) {
			t.Errorf("purge script lacks %q:\n%s", want, sh)
		}
	}
	if strings.Contains(sh, "/var/lib/postgresql") {
		t.Error("the purge script hardcodes the pg_hba.conf path")
	}
}

func TestForkQuitaRolesHeredados(t *testing.T) {
	ta := newTestApp(t)
	src := readyCopy(t, ta, "c1")
	// kling db role c1 -ro: el rol vive en la RAM y el disco de c1.
	ta.f.roRoles[src.ID] = []string{"agent", "otro"}
	ta.f.purged = nil
	copies, err := ta.fork(ctx, "c1", 3, "local")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range copies {
		assertPurgedBeforeReady(t, ta.f, c.ID)
	}
	// El origen no se toca: sus roles siguen.
	if len(ta.f.roRoles[src.ID]) != 2 {
		t.Fatalf("the source lost its roles: %v", ta.f.roRoles[src.ID])
	}
	for _, id := range ta.f.purged {
		if id == src.ID {
			t.Fatal("the source was purged")
		}
	}
}

// slowPurgeKling hace que la purga tarde, fuera del cerrojo del falso, y
// cuenta cuántas van a la vez.
type slowPurgeKling struct {
	*fakeKling
	cur, max atomic.Int32
}

func (k *slowPurgeKling) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	var in []byte
	if stdin != nil {
		in, _ = io.ReadAll(stdin)
	}
	if strings.Contains(string(in), purgeMarker) {
		n := k.cur.Add(1)
		for m := k.max.Load(); n > m && !k.max.CompareAndSwap(m, n); m = k.max.Load() {
		}
		time.Sleep(30 * time.Millisecond)
		k.cur.Add(-1)
	}
	return k.fakeKling.Run(ctx, strings.NewReader(string(in)), args...)
}

// Las copias de un fork se preparan a la vez, y cada una con un solo exec de
// purga (sin pg_isready aparte: Postgres ya acepta).
func TestForkPreparaLasCopiasALaVez(t *testing.T) {
	ta := newTestApp(t)
	readyCopy(t, ta, "c1")
	k := &slowPurgeKling{fakeKling: ta.f}
	ta.app.k = k
	copies, err := ta.fork(ctx, "c1", 4, "local")
	if err != nil {
		t.Fatal(err)
	}
	if m := k.max.Load(); m < 2 {
		t.Fatalf("at most %d copy prepared at once", m)
	}
	for _, c := range copies {
		assertPurgedBeforeReady(t, ta.f, c.ID)
		for _, call := range ta.f.calls {
			if indexOf(call.args, c.ID) >= 0 && strings.Contains(strings.Join(call.args, " "), "pg_isready") {
				t.Fatalf("copy %s: a separate pg_isready exec", shortID(c.ID))
			}
		}
	}
}

func TestUndoQuitaRolesHeredados(t *testing.T) {
	oa := newOpsApp(t)
	mc := oa.copyReady(t, "c1")
	oa.f.roRoles[mc.ID] = []string{"agent"}
	if _, err := oa.snapshot(ctx, "c1", "antes", "local", false); err != nil {
		t.Fatal(err)
	}
	nmc, err := oa.undo(ctx, "c1", "antes", "local")
	if err != nil {
		t.Fatal(err)
	}
	assertPurgedBeforeReady(t, oa.f, nmc.ID)
	// Y la clave del rol, que era del id viejo, no pasa al nuevo.
	if _, err := dbstate.ReadRolePassword(nmc.ID, "agent"); err == nil {
		t.Fatal("the new copy has a role password")
	}
}

func TestPurgaQueFallaNoEntregaLaCopia(t *testing.T) {
	ta := newTestApp(t)
	ta.f.snapRoles["pg"] = []string{"agent"}
	ta.f.purgeLeaves = true
	if _, err := ta.up(ctx, "pg", "c1", 0, "local"); err == nil || !strings.Contains(err.Error(), "read-only roles") {
		t.Fatalf("err = %v", err)
	}
	if len(ta.f.machines) != 0 {
		t.Fatalf("a copy with an inherited role survived: %v", ta.f.machines)
	}
	for _, c := range ta.f.calls {
		if c.labels[labelState] == stateReady {
			t.Fatal("marked ready")
		}
	}
	d, _ := dbstate.Dir()
	if ents, _ := os.ReadDir(filepath.Join(d, dbstate.CopiesDir)); len(ents) != 0 {
		t.Fatalf("password dirs left: %v", ents)
	}
}

// runPurge ejecuta purgeScript de verdad con /bin/sh y un `su` falso que hace
// de Postgres: pg_isready sale con ready (0 o 1); el psql de la purga apunta
// el SQL que le llega por stdin y contesta left como cuenta final; SHOW
// hba_file y pg_reload_conf, como en runHBA.
func runPurge(t *testing.T, hba string, ready int, left string) (out, sql, hbaOut string, reloads int, err error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "pg_hba.conf")
	if err := os.WriteFile(f, []byte(hba), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	rel, sqlF := filepath.Join(dir, "reloads"), filepath.Join(dir, "sql")
	su := "#!/bin/sh\ncase \"$5\" in\n" +
		"  *'SHOW hba_file'*) echo '" + f + "' ;;\n" +
		"  *pg_reload_conf*) echo x >> '" + rel + "'; echo t ;;\n" +
		"  *pg_isready*) exit " + strconv.Itoa(ready) + " ;;\n" +
		"  '" + psqlSuper + "') cat > '" + sqlF + "'; echo 0; echo '" + left + "' ;;\n" +
		"  *) echo \"unexpected: $5\" >&2; exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "su"), []byte(su), 0o755); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("/bin/sh", "-s")
	c.Stdin = strings.NewReader(purgeScript("appdb"))
	c.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
	b, err := c.Output()
	s, _ := os.ReadFile(sqlF)
	h, _ := os.ReadFile(f)
	if r, rerr := os.ReadFile(rel); rerr == nil {
		reloads = strings.Count(string(r), "x")
	}
	return string(b), string(s), string(h), reloads, err
}

// purgeScript hace en un exec lo que antes eran tres, en el mismo orden y con
// las mismas paradas.
func TestPurgeScriptDeVerdad(t *testing.T) {
	sucio := hbaBase + `host "appdb" "agent" 0.0.0.0/0 scram-sha-256 # kling-db` + "\n"

	out, sql, hba, reloads, err := runPurge(t, sucio, 0, "0")
	if err != nil || lastLine(out) != "0" {
		t.Fatalf("purge: %v, %q", err, out)
	}
	if sql != fmt.Sprintf(purgeROSQL, "appdb") {
		t.Fatalf("the SQL that reached psql is not purgeROSQL:\n%s", sql)
	}
	if hba != hbaBase || reloads != 1 {
		t.Fatalf("pg_hba.conf (%d reloads):\n%s", reloads, hba)
	}

	// Postgres aún no acepta: no toca nada y lo dice.
	out, sql, hba, _, err = runPurge(t, sucio, 1, "0")
	if err != nil || lastLine(out) != purgeNotReady || sql != "" || hba != sucio {
		t.Fatalf("not ready: %v, %q, sql %q", err, out, sql)
	}

	// Queda un rol: la cuenta, y pg_hba.conf sin tocar (como antes).
	out, _, hba, _, err = runPurge(t, sucio, 0, "1")
	if err != nil || lastLine(out) != "1" || hba != sucio {
		t.Fatalf("one left: %v, %q\n%s", err, out, hba)
	}
}
