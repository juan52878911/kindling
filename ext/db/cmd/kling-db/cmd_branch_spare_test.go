package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// sparesOf son las reservas de la rama branch del repo de la prueba (vivas).
func (ta *testApp) sparesOf(t *testing.T, branch string) []*api.Machine {
	t.Helper()
	ctx := context.Background()
	ri, err := ta.repo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, spares, err := ta.repoState(ctx, ri, defaultOwner)
	if err != nil {
		t.Fatal(err)
	}
	var out []*api.Machine
	for _, mc := range spares[branchKey(branch)] {
		out = append(out, ta.f.find(mc.ID))
	}
	return out
}

// switchTo hace checkout de branch (-b si nueva) y lo que haría el gancho.
func (ta *testApp) switchTo(t *testing.T, dir, branch string, nueva bool) {
	t.Helper()
	if nueva {
		gitT(t, dir, "checkout", "-q", "-b", branch)
	} else {
		gitT(t, dir, "checkout", "-q", branch)
	}
	if err := ta.branchSwitch(context.Background(), "", defaultOwner, "", false); err != nil {
		t.Fatalf("switch to %s: %v\n%s", branch, err, ta.err)
	}
}

// Al salir de main queda una reserva suya, congelada; al volver a main no se
// toca; y la siguiente rama nueva se la queda en vez de ramificar.
func TestBranchReservaSeAdoptaSiElPadreNoCambio(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	mainC := ta.copyOf(t, "main")
	ta.switchTo(t, dir, "a", true)
	sp := ta.sparesOf(t, "main")
	if len(sp) != 1 {
		t.Fatalf("after leaving main: %d spares, want 1", len(sp))
	}
	spare := sp[0]
	if spare.State != api.StateWarm || spare.Labels[labelState] != stateReady || spare.Labels[labelBranch] != "" ||
		spare.Labels[labelSpareFP] != fpDefault || password(t, spare.ID) == "" {
		t.Fatalf("spare: state %s labels %v", spare.State, spare.Labels)
	}
	if mainC.State != api.StateWarm {
		t.Errorf("main is %s: it should be frozen once left", mainC.State)
	}

	// Volver a main no gasta la reserva ni saca otra.
	ta.switchTo(t, dir, "main", false)
	if sp := ta.sparesOf(t, "main"); len(sp) != 1 || sp[0].ID != spare.ID {
		t.Fatalf("back on main the spare changed: %v", sp)
	}

	forks := len(ta.f.forks())
	ta.switchTo(t, dir, "b", true)
	b := ta.copyOf(t, "b")
	if b == nil || b.ID != spare.ID {
		t.Fatalf("branch b = %+v; want the spare %s", b, spare.ID)
	}
	if b.State != api.StateRunning || b.Labels[labelSpare] != "" || b.Labels[labelSpareFP] != "" || b.Labels[labelBranch] != branchKey("b") {
		t.Errorf("adopted copy: state %s labels %v", b.State, b.Labels)
	}
	// La única rama nueva ramificada después es la reserva nueva que deja
	// settle al salir de main (y no la rama b).
	if got := ta.f.forks()[forks:]; len(got) != 1 || got[0] != mainC.ID {
		t.Errorf("forks after the adoption = %v; want one new spare of main", got)
	}
	if sp := ta.sparesOf(t, "main"); len(sp) != 1 || sp[0].ID == spare.ID {
		t.Errorf("after the adoption main should have a new spare: %v", sp)
	}
	env, _ := os.ReadFile(filepath.Join(dir, ".git", branchEnvFile))
	if !strings.Contains(string(env), password(t, b.ID)) || strings.Contains(string(env), password(t, mainC.ID)) {
		t.Errorf("env of b:\n%s", env)
	}
	if !strings.Contains(ta.err.String(), "took a spare copy of main") {
		t.Errorf("stderr:\n%s", ta.err)
	}
}

// Si main cambió desde que se sacó la reserva, la rama nueva sale del fork de
// main (con los datos de ahora) y la reserva vieja se tira.
func TestBranchReservaNoSeAdoptaSiElPadreCambio(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	mainC := ta.copyOf(t, "main")
	ta.switchTo(t, dir, "a", true)
	old := ta.sparesOf(t, "main")
	if len(old) != 1 {
		t.Fatalf("%d spares", len(old))
	}
	ta.switchTo(t, dir, "main", false)
	ta.f.fp[mainC.ID] = "11111111111111111111111111111111" // alguien escribió en main
	ta.switchTo(t, dir, "b", true)
	b := ta.copyOf(t, "b")
	if b == nil || b.ID == old[0].ID {
		t.Fatalf("b took the stale spare")
	}
	if ta.f.find(old[0].ID) != nil {
		t.Error("the stale spare was not removed")
	}
	sp := ta.sparesOf(t, "main")
	if len(sp) != 1 || sp[0].Labels[labelSpareFP] != ta.f.fp[mainC.ID] {
		t.Errorf("new spare: %v", sp)
	}
}

// El padre congelado se descongela para leer su huella; con ella igual, se
// adopta la reserva (desde una rama que no es main).
func TestBranchReservaConElPadreCongelado(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	ta.switchTo(t, dir, "a", true)
	spare := ta.sparesOf(t, "main")[0]
	if ta.copyOf(t, "main").State != api.StateWarm {
		t.Fatal("main not frozen")
	}
	ta.switchTo(t, dir, "c", true)
	if c := ta.copyOf(t, "c"); c == nil || c.ID != spare.ID {
		t.Fatalf("c = %+v, want the spare", c)
	}
	// Y main, que se descongeló para mirarla, vuelve a quedar congelada.
	if st := ta.copyOf(t, "main").State; st != api.StateWarm {
		t.Errorf("main is %s", st)
	}
}

// Mientras un worktree tiene activa main no se le saca reserva: pausaría la
// base que se está usando.
func TestBranchReservaNoPausaLaRamaActiva(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	ri, _ := ta.repo(ctx)
	if err := ta.settle(ctx, ri, defaultOwner); err != nil {
		t.Fatal(err)
	}
	if n := len(ta.f.forks()); n != 0 || len(ta.sparesOf(t, "main")) != 0 {
		t.Fatalf("forks %d spares %d with main checked out", n, len(ta.sparesOf(t, "main")))
	}
	_ = dir
}

func TestBranchReservaDesactivada(t *testing.T) {
	t.Setenv(spareEnv, "0")
	ta, dir := branchTestApp(t)
	if err := ta.branch(context.Background(), "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	ta.switchTo(t, dir, "a", true)
	if sp := ta.sparesOf(t, "main"); len(sp) != 0 {
		t.Fatalf("%s=0 and there are spares: %v", spareEnv, sp)
	}
}

// Una reserva que ya adoptó una rama no se congela ni se borra como reserva.
func TestWithSpareRespetaLaAdopcion(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	ta.switchTo(t, dir, "a", true)
	spare := ta.sparesOf(t, "main")[0]
	ri, _ := ta.repo(ctx)
	// La adopta una rama (lo que haría un -switch concurrente).
	if err := ta.f.SetLabels(ctx, spare.ID, map[string]string{labelBranch: branchKey("x"), labelSpare: ""}); err != nil {
		t.Fatal(err)
	}
	ta.dropSpare(ctx, ri, spare.ID, branchKey("main"), defaultOwner)
	if ta.f.find(spare.ID) == nil {
		t.Fatal("dropSpare removed a copy that a branch had adopted")
	}
	// Ni de otro dueño.
	if err := ta.f.SetLabels(ctx, spare.ID, map[string]string{labelBranch: "", labelSpare: branchKey("main"), labelOwner: "otro"}); err != nil {
		t.Fatal(err)
	}
	ta.dropSpare(ctx, ri, spare.ID, branchKey("main"), defaultOwner)
	if ta.f.find(spare.ID) == nil {
		t.Fatal("dropSpare removed another owner's copy")
	}
}

func TestBranchRmYLsConReserva(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	ta.switchTo(t, dir, "a", true)
	spare := ta.sparesOf(t, "main")[0]
	ta.out.Reset()
	if err := ta.branchLs(ctx, defaultOwner, true); err != nil {
		t.Fatal(err)
	}
	var rows []branchRow
	if err := json.Unmarshal(ta.out.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.Spare && r.Branch == "main" && r.Copy == spare.Name {
			found = true
		}
	}
	if !found {
		t.Errorf("-ls does not show the spare: %+v", rows)
	}
	if err := ta.branchRm(ctx, "main", defaultOwner); err != nil {
		t.Fatal(err)
	}
	if ta.f.find(spare.ID) != nil {
		t.Error("-rm main left its spare behind")
	}
}

func TestFingerprintRechazaRespuestaRara(t *testing.T) {
	ta, _ := branchTestApp(t)
	mc := ta.f.newMachine("x", map[string]string{labelGolden: "pg", labelOwner: defaultOwner})
	ta.f.fp[mc.ID] = "no es una huella"
	if _, err := ta.fingerprint(context.Background(), mc); err == nil {
		t.Fatal("accepted a bad fingerprint")
	}
	delete(ta.f.fp, mc.ID)
	fp, err := ta.fingerprint(context.Background(), mc)
	if err != nil || fp != fpDefault {
		t.Fatalf("fp %q %v", fp, err)
	}
	// La consulta va por stdin, en la base de la aplicación, sin nada en argv.
	last := ta.f.calls[len(ta.f.calls)-1]
	if !strings.Contains(last.stdin, `\connect appdb`) || strings.Contains(strings.Join(last.args, " "), "pg_current_snapshot") {
		t.Errorf("call: %+v", last)
	}
}

// Con KLING_DB_BRANCH_KEEP_PAUSED=1 la última rama que se deja queda pausada y
// las anteriores, congeladas (también la que estaba pausada).
func TestBranchKeepPaused(t *testing.T) {
	t.Setenv(keepPausedEnv, "1")
	t.Setenv(spareEnv, "0")
	ta, dir := branchTestApp(t)
	now := int64(1000)
	ta.now = func() time.Time { now++; return time.Unix(now, 0) }
	if err := ta.branch(context.Background(), "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	ta.switchTo(t, dir, "a", true)
	if st := ta.copyOf(t, "main").State; st != api.StatePaused {
		t.Fatalf("main is %s, want paused", st)
	}
	ta.switchTo(t, dir, "b", true)
	if m, a := ta.copyOf(t, "main").State, ta.copyOf(t, "a").State; m != api.StateWarm || a != api.StatePaused {
		t.Fatalf("main %s (want frozen), a %s (want paused)", m, a)
	}
	// Volver a a: la reanuda; b queda pausada y main sigue congelada.
	ta.switchTo(t, dir, "a", false)
	if a, b, m := ta.copyOf(t, "a").State, ta.copyOf(t, "b").State, ta.copyOf(t, "main").State; a != api.StateRunning || b != api.StatePaused || m != api.StateWarm {
		t.Fatalf("a %s b %s main %s", a, b, m)
	}
	for _, bad := range []string{"-1", "x", ""} {
		t.Setenv(keepPausedEnv, bad)
		if keepPaused() != 0 {
			t.Errorf("%q -> %d", bad, keepPaused())
		}
	}
	t.Setenv(keepPausedEnv, "100")
	if keepPaused() != 8 {
		t.Errorf("not capped: %d", keepPaused())
	}
}
