package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/pkg/api"
)

func writeMigs(t *testing.T, files map[string]string) string {
	t.Helper()
	d := t.TempDir()
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(d, n), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// ── rehearse ─────────────────────────────────────────────────────────────────

func TestRehearseCopiaNoTocaOrigenYDestruye(t *testing.T) {
	oa := newOpsApp(t)
	src := oa.copyReady(t, "c1")
	oa.f.calls = nil
	dir := writeMigs(t, map[string]string{
		"002_b.sql": "ALTER TABLE t ADD c int; -- GROW",
		"001_a.sql": "CREATE TABLE t(); -- GROW",
	})
	rep, err := oa.rehearse(ctx, "c1", "local", dir, nil, 5*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK || rep.SourceKind != "copy" || len(rep.Files) != 2 {
		t.Fatalf("report %+v", rep)
	}
	// En orden de nombre, con tamaños encadenados.
	if rep.Files[0].File != "001_a.sql" || rep.Files[1].File != "002_b.sql" {
		t.Fatalf("order %+v", rep.Files)
	}
	if rep.Files[0].SizeAfter-rep.Files[0].SizeBefore != 1<<20 || rep.Files[1].SizeBefore != rep.Files[0].SizeAfter {
		t.Fatalf("sizes %+v", rep.Files)
	}
	// Solo queda el origen: la copia desechable se destruyó con su contraseña.
	if len(oa.f.machines) != 1 || oa.f.machines[src.ID] == nil {
		t.Fatalf("machines %v", oa.f.machines)
	}
	d, _ := dbstate.Dir()
	if ents, _ := os.ReadDir(filepath.Join(d, dbstate.CopiesDir)); len(ents) != 1 {
		t.Fatalf("password dirs: %v", ents)
	}
	// Nada se ejecutó en el origen.
	for _, c := range oa.f.calls {
		if len(c.args) > 0 && c.args[0] == "exec" && indexOf(c.args, src.ID) >= 0 {
			t.Fatalf("the source was touched: %v", c.args)
		}
	}
	if len(oa.o.applied[src.ID]) != 0 {
		t.Fatal("migrations applied to the source")
	}
	// Como el rol de la aplicación DESDE LA AUTENTICACIÓN (peer por el socket,
	// no postgres con role=, que RESET ROLE desharía), con lock_timeout y
	// ON_ERROR_STOP; y el mapa peer se pone antes, en la copia desechable.
	var found, peer bool
	for _, c := range oa.f.calls {
		j := strings.Join(c.args, " ")
		if strings.HasSuffix(j, "sh -s") && strings.Contains(c.stdin, "peer map="+rehearseMap) {
			if indexOf(c.args, src.ID) >= 0 || !strings.Contains(c.stdin, "local all app peer") {
				t.Fatalf("peer setup %v\n%s", c.args, c.stdin)
			}
			peer = true
		}
		if strings.Contains(j, "PGOPTIONS") {
			found = true
			if !peer {
				t.Fatal("migration run before the peer map was set up")
			}
			if strings.Contains(j, "role=") || !strings.Contains(j, "-h /run/postgresql -U app -d appdb") ||
				!strings.Contains(j, "lock_timeout=5000") || !strings.Contains(j, "ON_ERROR_STOP=1") {
				t.Fatalf("migration cmd %s", j)
			}
		}
	}
	if !found {
		t.Fatal("no migration executed")
	}
}

func TestRehearseGoldenYKeep(t *testing.T) {
	oa := newOpsApp(t)
	dir := writeMigs(t, map[string]string{"1.sql": "SELECT 1;"})
	rep, err := oa.rehearse(ctx, "pg", "local", dir, nil, time.Second, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SourceKind != "template" || rep.KeptCopy == "" || len(oa.f.machines) != 1 {
		t.Fatalf("%+v %v", rep, oa.f.machines)
	}
	var buf bytes.Buffer
	if err := writeRehearse(&buf, rep, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "kept the copy "+rep.KeptCopy) || !strings.Contains(buf.String(), "1.sql") {
		t.Fatalf("table:\n%s", buf.String())
	}
}

func TestRehearseLockTimeoutYFallo(t *testing.T) {
	oa := newOpsApp(t)
	dir := writeMigs(t, map[string]string{
		"1_ok.sql":   "SELECT 1;",
		"2_lock.sql": "ALTER TABLE secretos ...; -- LOCK",
		"3_next.sql": "SELECT 2;",
	})
	rep, err := oa.rehearse(ctx, "pg", "local", dir, nil, 3*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK {
		t.Fatal("a failed file must make the report not ok")
	}
	f := rep.Files[1]
	if f.Status != "failed" || !f.WaitedForLocks || f.WouldBlockSeconds != 3 {
		t.Fatalf("lock file %+v", f)
	}
	if rep.Files[2].Status != "skipped" || len(oa.o.applied) != 1 {
		t.Fatalf("after a failure nothing else runs: %+v", rep.Files)
	}
	var buf bytes.Buffer
	_ = writeRehearse(&buf, rep, false)
	if !strings.Contains(buf.String(), "would block 3 s in production") {
		t.Fatalf("table:\n%s", buf.String())
	}
	// El error no cita la sentencia (LINE 1: ...): puede llevar datos. La
	// lista de sentencias con locks fuertes sí la nombra, normalizada (sin
	// literales), como observe.
	if strings.Contains(f.Error, "secretos") {
		t.Fatalf("the error quotes the statement: %q", f.Error)
	}
	if len(f.StrongStatements) != 1 || !strings.Contains(f.StrongStatements[0], "ACCESS EXCLUSIVE") {
		t.Fatalf("strong statements: %v", f.StrongStatements)
	}
	// Se destruyó igual.
	if len(oa.f.machines) != 0 {
		t.Fatalf("machines %v", oa.f.machines)
	}
}

func TestRehearseErrorSQLSinSentencia(t *testing.T) {
	oa := newOpsApp(t)
	dir := writeMigs(t, map[string]string{"1.sql": "BOOM 'clave-secreta' -- BAD"})
	rep, err := oa.rehearse(ctx, "pg", "local", dir, nil, time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rep.Files[0].Error, "syntax error") || strings.Contains(rep.Files[0].Error, "clave-secreta") {
		t.Fatalf("error %q", rep.Files[0].Error)
	}
	var buf bytes.Buffer
	_ = writeRehearse(&buf, rep, true)
	var back rehearseReport
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil || back.OK || back.Files[0].Status != "failed" {
		t.Fatalf("json %v %s", err, buf.String())
	}
}

func TestRehearseMuestreoDeLocks(t *testing.T) {
	oa := newOpsApp(t)
	lockSampleEvery = 5 * time.Millisecond
	oa.o.waiters = 1
	dir := writeMigs(t, map[string]string{"1.sql": "SELECT pg_sleep(1); -- SLOW"})
	rep, err := oa.rehearse(ctx, "pg", "local", dir, nil, time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	f := rep.Files[0]
	if f.Status != "ok" || !f.WaitedForLocks || f.LockWaitSeconds <= 0 || f.WouldBlockSeconds != 0 {
		t.Fatalf("%+v", f)
	}
}

func TestRehearseValidaAntesDeCrear(t *testing.T) {
	oa := newOpsApp(t)
	good := writeMigs(t, map[string]string{"1.sql": "SELECT 1;"})
	link := t.TempDir()
	if err := os.Symlink("/etc/passwd", filepath.Join(link, "1.sql")); err != nil {
		t.Skip("symlinks:", err)
	}
	for name, tc := range map[string]struct {
		dir string
		lt  time.Duration
	}{
		"sin sql":        {t.TempDir(), time.Second},
		"no existe":      {"/nonexistent-dir-xyz", time.Second},
		"enlace":         {link, time.Second},
		"lock demasiado": {good, time.Millisecond},
		"lock enorme":    {good, 24 * time.Hour},
	} {
		if _, err := oa.rehearse(ctx, "pg", "local", tc.dir, nil, tc.lt, false); err == nil {
			t.Fatalf("%s: want error", name)
		}
	}
	if len(oa.f.machines) != 0 {
		t.Fatalf("something was created: %v", oa.f.machines)
	}
}

func TestRehearseOrigenAjenoOCongelado(t *testing.T) {
	oa := newOpsApp(t)
	mc := oa.copyReady(t, "c1")
	dir := writeMigs(t, map[string]string{"1.sql": "SELECT 1;"})
	if _, err := oa.rehearse(ctx, "c1", "otro", dir, nil, time.Second, false); err == nil {
		t.Fatal("foreign copy accepted")
	}
	oa.f.machines[mc.ID].State = api.StatePaused
	if _, err := oa.rehearse(ctx, "c1", "local", dir, nil, time.Second, false); err == nil || !strings.Contains(err.Error(), "thaw it first") {
		t.Fatalf("paused source: %v", err)
	}
	if oa.f.machines[mc.ID].State != api.StatePaused {
		t.Fatal("the source was thawed")
	}
}

// ── rotate ───────────────────────────────────────────────────────────────────

func TestRotateCambiaClaveYFichero(t *testing.T) {
	oa := newOpsApp(t)
	mc := oa.copyReady(t, "c1")
	old := password(t, mc.ID)
	if _, err := oa.rotateCopy(ctx, "c1", "local"); err != nil {
		t.Fatal(err)
	}
	pw := password(t, mc.ID)
	if pw == old {
		t.Fatal("password not rotated")
	}
	assertVerifierMatches(t, oa.f.verifier[mc.ID], pw)
	assertNoLeak(t, oa.f, pw)
	assertNoLeak(t, oa.f, old)
	p, _ := dbstate.PasswordPath(mc.ID)
	if _, err := os.Stat(filepath.Dir(p) + "/password.new"); err == nil {
		t.Fatal("password.new left behind")
	}
}

func TestRotateFallaYLaAnteriorSigueValiendo(t *testing.T) {
	for name, fail := range map[string]map[int]bool{
		"psql falla":             {2: true}, // el 1 fue el de up
		"plazo vencido y aplicó": {2: true},
	} {
		t.Run(name, func(t *testing.T) {
			oa := newOpsApp(t)
			mc := oa.copyReady(t, "c1")
			old := password(t, mc.ID)
			oa.o.alterCalls = 0
			oa.o.failAlter = map[int]bool{1: fail[2]}
			if _, err := oa.rotateCopy(ctx, "c1", "local"); err == nil {
				t.Fatal("want error")
			}
			if got := password(t, mc.ID); got != old {
				t.Fatal("the stored password changed although the rotation failed")
			}
			// La base vuelve a tener el verificador de la clave anterior.
			assertVerifierMatches(t, oa.f.verifier[mc.ID], old)
			p, _ := dbstate.PasswordPath(mc.ID)
			if _, err := os.Stat(filepath.Dir(p) + "/password.new"); err == nil {
				t.Fatal("password.new left behind")
			}
		})
	}
}

func TestRotateVerificadorDistinto(t *testing.T) {
	oa := newOpsApp(t)
	mc := oa.copyReady(t, "c1")
	old := password(t, mc.ID)
	oa.f.wrongVerifier = true
	if _, err := oa.rotateCopy(ctx, "c1", "local"); err == nil {
		t.Fatal("want error")
	}
	if password(t, mc.ID) != old {
		t.Fatal("password changed")
	}
}

func TestRotateExigeDuenoYListaYPropia(t *testing.T) {
	oa := newOpsApp(t)
	mc := oa.copyReady(t, "c1")
	if _, err := oa.rotateCopy(ctx, "c1", "otro"); err == nil {
		t.Fatal("foreign owner accepted")
	}
	oa.f.machines[mc.ID].Labels[labelState] = statePreparing
	if _, err := oa.rotateCopy(ctx, "c1", "local"); err == nil {
		t.Fatal("not-ready copy accepted")
	}
	oa.f.machines[mc.ID].Labels[labelState] = stateReady
	if err := dbstate.Remove(mc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := oa.rotateCopy(ctx, "c1", "local"); err == nil {
		t.Fatal("a copy without a host password accepted")
	}
}

// ── snapshot, snapshots, undo ────────────────────────────────────────────────

func TestSnapshotYUndo(t *testing.T) {
	oa := newOpsApp(t)
	mc := oa.copyReady(t, "c1")
	tpl, err := oa.snapshot(ctx, "c1", "antes", "local", false)
	if err != nil {
		t.Fatal(err)
	}
	s := oa.f.snaps[tpl]
	if s == nil || s.Labels[labelGolden] != "pg" || s.Labels[labelSnapshotOf] != mc.ID || s.Labels[labelOwner] != "local" {
		t.Fatalf("template %+v", s)
	}
	// La copia sigue viva y lista.
	if oa.f.machines[mc.ID] == nil {
		t.Fatal("snapshot removed the copy")
	}
	list, err := oa.listSnapshots(ctx, "c1", "local")
	if err != nil || len(list) != 1 || list[0].Name != "antes" {
		t.Fatalf("list %+v %v", list, err)
	}

	oldPw := password(t, mc.ID)
	nmc, err := oa.undo(ctx, "c1", "antes", "local")
	if err != nil {
		t.Fatal(err)
	}
	if nmc.ID == mc.ID || nmc.Name != "c1" || oa.f.machines[mc.ID] != nil || oa.f.machines[nmc.ID] == nil {
		t.Fatalf("undo: %+v", nmc)
	}
	live := oa.f.machines[nmc.ID]
	if live.From != tpl || live.Labels[labelGolden] != "pg" || live.Labels[labelOwner] != "local" || live.Labels[labelState] != stateReady {
		t.Fatalf("labels %v from %s", live.Labels, live.From)
	}
	// Contraseña nueva, propia de la copia nueva; la de la vieja se fue.
	pw := password(t, nmc.ID)
	if pw == oldPw {
		t.Fatal("undo kept the old password")
	}
	assertVerifierMatches(t, oa.f.verifier[nmc.ID], pw)
	if _, err := dbstate.ReadPassword(mc.ID); err == nil {
		t.Fatal("the old copy's password file survived")
	}
	// La copia nueva conserva sus puntos de guardado.
	list, err = oa.listSnapshots(ctx, "c1", "local")
	if err != nil || len(list) != 1 || list[0].Instances != 1 {
		t.Fatalf("after undo: %+v %v", list, err)
	}
	// Y se puede volver a hacer undo (sin nombre = el último).
	again, err := oa.undo(ctx, "c1", "", "local")
	if err != nil || again.ID == nmc.ID {
		t.Fatalf("second undo: %v", err)
	}
}

func TestSnapshotLimiteYRepetido(t *testing.T) {
	oa := newOpsApp(t)
	oa.copyReady(t, "c1")
	for i := 0; i < maxSnapshots; i++ {
		if _, err := oa.snapshot(ctx, "c1", "s"+string(rune('a'+i)), "local", false); err != nil {
			t.Fatalf("snapshot %d: %v", i, err)
		}
	}
	if _, err := oa.snapshot(ctx, "c1", "uno-mas", "local", false); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("limit: %v", err)
	}
	// Repetido (con espacio: quitamos uno para probar el nombre).
	if err := oa.snapshotRemove(ctx, "c1", "sa", "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := oa.snapshot(ctx, "c1", "sb", "local", false); err == nil || !strings.Contains(err.Error(), "already has") {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestSnapshotRmYLosVivos(t *testing.T) {
	oa := newOpsApp(t)
	oa.copyReady(t, "c1")
	if _, err := oa.snapshot(ctx, "c1", "p1", "local", false); err != nil {
		t.Fatal(err)
	}
	if err := oa.snapshotRemove(ctx, "c1", "nope", "local"); err == nil {
		t.Fatal("removing a missing snapshot")
	}
	if err := oa.snapshotRemove(ctx, "c1", "p1", "local"); err != nil {
		t.Fatal(err)
	}
	if len(oa.f.snaps) != 1 { // solo el golden "pg"
		t.Fatalf("snaps %v", oa.f.snaps)
	}
	// Con una copia viva nacida del punto, kling se niega y el punto sigue.
	if _, err := oa.snapshot(ctx, "c1", "p2", "local", false); err != nil {
		t.Fatal(err)
	}
	if _, err := oa.undo(ctx, "c1", "p2", "local"); err != nil {
		t.Fatal(err)
	}
	if err := oa.snapshotRemove(ctx, "c1", "p2", "local"); err == nil {
		t.Fatal("removed a snapshot with a live copy")
	}
	if list, _ := oa.listSnapshots(ctx, "c1", "local"); len(list) != 1 {
		t.Fatalf("list %+v", list)
	}
}

func TestSnapshotIdentidadYDueno(t *testing.T) {
	oa := newOpsApp(t)
	c1 := oa.copyReady(t, "c1")
	if _, err := oa.snapshot(ctx, "c1", "p1", "local", false); err != nil {
		t.Fatal(err)
	}
	// Otro dueño no puede guardar, listar, borrar ni deshacer.
	if _, err := oa.snapshot(ctx, "c1", "x", "otro", false); err == nil {
		t.Fatal("foreign snapshot")
	}
	if _, err := oa.listSnapshots(ctx, "c1", "otro"); err == nil {
		t.Fatal("foreign list")
	}
	if err := oa.snapshotRemove(ctx, "c1", "p1", "otro"); err == nil {
		t.Fatal("foreign rm")
	}
	if _, err := oa.undo(ctx, "c1", "p1", "otro"); err == nil {
		t.Fatal("foreign undo")
	}
	// Un fork de c1 tiene otro nombre y no ve sus puntos.
	forks, err := oa.fork(ctx, "c1", 1, "local")
	if err != nil {
		t.Fatal(err)
	}
	if l, err := oa.listSnapshots(ctx, forks[0].Name, "local"); err != nil || len(l) != 0 {
		t.Fatalf("a fork sees the snapshots of its source: %v %v", l, err)
	}
	if _, err := oa.undo(ctx, forks[0].Name, "p1", "local"); err == nil {
		t.Fatal("a fork undid to the snapshot of its source")
	}
	// Una copia distinta con el mismo nombre (rm y up) no hereda los puntos
	// (id nuevo => linaje nuevo).
	if err := oa.remove(ctx, oa.f.machines[c1.ID]); err != nil {
		t.Fatal(err)
	}
	oa.copyReady(t, "c1")
	if l, _ := oa.listSnapshots(ctx, "c1", "local"); len(l) != 0 {
		t.Fatalf("a new copy with the same name inherited snapshots: %v", l)
	}
	// Una plantilla ajena con el mismo prefijo pero etiquetas falsas no cuenta.
	oa.f.snaps[snapTemplatePrefix("local", "c1")+"falso"] = &api.Snapshot{Name: snapTemplatePrefix("local", "c1") + "falso",
		Labels: map[string]string{labelOwner: "otro", labelGolden: "pg"}}
	if l, _ := oa.listSnapshots(ctx, "c1", "local"); len(l) != 0 {
		t.Fatalf("a forged template counted: %v", l)
	}
}

func TestSnapshotConexionesAbiertasYForce(t *testing.T) {
	oa := newOpsApp(t)
	oa.copyReady(t, "c1")
	oa.o.clients = 2
	if _, err := oa.snapshot(ctx, "c1", "p1", "local", false); err == nil || !strings.Contains(err.Error(), "2 client connection") {
		t.Fatalf("open connections: %v", err)
	}
	if len(oa.f.snaps) != 1 {
		t.Fatal("a template was created")
	}
	if _, err := oa.snapshot(ctx, "c1", "p1", "local", true); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotFallaAlGuardar(t *testing.T) {
	oa := newOpsApp(t)
	mc := oa.copyReady(t, "c1")
	oa.o.failSave = true
	if _, err := oa.snapshot(ctx, "c1", "p1", "local", false); err == nil {
		t.Fatal("want error")
	}
	if oa.f.machines[mc.ID] == nil || len(oa.f.snaps) != 1 {
		t.Fatal("state changed after a failed save")
	}
}

func TestSnapshotNombresInvalidos(t *testing.T) {
	oa := newOpsApp(t)
	oa.copyReady(t, "c1")
	for _, n := range []string{"", "A", "a b", "../x", "-a", strings.Repeat("a", 41), "a/b", "a;b"} {
		if _, err := oa.snapshot(ctx, "c1", n, "local", false); err == nil {
			t.Fatalf("name %q accepted", n)
		}
		if _, err := oa.undo(ctx, "c1", n, "local"); n != "" && err == nil {
			t.Fatalf("undo name %q accepted", n)
		}
	}
	// Todo nombre válido da un nombre de plantilla válido para el núcleo.
	if n := snapTemplatePrefix("local", strings.Repeat("x", 64)) + strings.Repeat("a", 40); !namePattern.MatchString(n) {
		t.Fatalf("template name %q is not a valid template name", n)
	}
}

func TestUndoSinPuntos(t *testing.T) {
	oa := newOpsApp(t)
	mc := oa.copyReady(t, "c1")
	if _, err := oa.undo(ctx, "c1", "", "local"); err == nil {
		t.Fatal("want error")
	}
	if oa.f.machines[mc.ID] == nil {
		t.Fatal("the copy was removed without a snapshot to return to")
	}
}

func TestUndoFalloDeRotacionDejaElPunto(t *testing.T) {
	oa := newOpsApp(t)
	oa.copyReady(t, "c1")
	if _, err := oa.snapshot(ctx, "c1", "p1", "local", false); err != nil {
		t.Fatal(err)
	}
	oa.f.failRotation = oa.f.rotations + 1
	_, err := oa.undo(ctx, "c1", "p1", "local")
	if err == nil || !strings.Contains(err.Error(), "snapshot is intact") {
		t.Fatalf("undo: %v", err)
	}
	if len(oa.f.machines) != 0 {
		t.Fatalf("machines %v", oa.f.machines)
	}
	if len(oa.f.snaps) != 2 {
		t.Fatalf("the snapshot is gone: %v", oa.f.snaps)
	}
}

func TestEtiquetasNuevasCumplenKeyPattern(t *testing.T) {
	if !api.KeyPattern.MatchString(labelSnapshotOf) {
		t.Fatalf("%s is not a valid label key", labelSnapshotOf)
	}
}

func TestManifiestoIncluyeOperaciones(t *testing.T) {
	have := map[string]bool{}
	m := manifest()
	for _, c := range m.Commands {
		have[c.Name] = true
	}
	for _, n := range []string{"rehearse", "rotate", "snapshot", "snapshots", "undo"} {
		if !have[n] {
			t.Fatalf("manifest lacks %s", n)
		}
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
}
