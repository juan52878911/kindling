package main

// Una copia nueva no hereda los roles de `kling db role`: prepare los quita
// (con su línea de pg_hba.conf) antes de darla por lista, venga de up, fork o
// undo.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
)

// assertPurgedBeforeReady: en la copia id se purgaron los roles (SQL) y
// pg_hba.conf (sh) ANTES de marcarla ready, y ya no le queda ningún rol.
func assertPurgedBeforeReady(t *testing.T, f *fakeKling, id string) {
	t.Helper()
	sqlAt, hbaAt, readyAt := -1, -1, -1
	for i, c := range f.calls {
		switch {
		case c.ref == id && c.labels[labelState] == stateReady:
			readyAt = i
		case len(c.args) > 0 && c.args[0] == "exec" && indexOf(c.args, id) >= 0 && strings.Contains(c.stdin, purgeMarker):
			if strings.HasSuffix(strings.Join(c.args, " "), "sh -s") {
				hbaAt = i
			} else {
				sqlAt = i
			}
		}
	}
	if sqlAt < 0 || hbaAt < 0 || readyAt < 0 || sqlAt > readyAt || hbaAt > readyAt {
		t.Fatalf("copy %s: purge sql@%d hba@%d, ready@%d", shortID(id), sqlAt, hbaAt, readyAt)
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
	var sql, sh string
	for _, c := range ta.f.calls {
		if strings.Contains(c.stdin, purgeMarker) {
			if strings.HasSuffix(strings.Join(c.args, " "), "sh -s") {
				sh = c.stdin
			} else {
				sql = c.stdin
			}
		}
	}
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
