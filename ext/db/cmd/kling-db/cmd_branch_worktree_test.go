package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/pkg/api"
)

// otherApp es un segundo kling-db contra el mismo daemon falso y el mismo
// estado, con su propio directorio de trabajo y sus propias salidas: como otro
// proceso en otro worktree.
func (ta *testApp) otherApp(cwd string) *testApp {
	o := &testApp{f: ta.f, out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	cp := *ta.app
	cp.stdout, cp.stderr, cp.cwd = o.out, o.err, cwd
	o.app = &cp
	return o
}

func TestBranchWorktreesCompartenCopia(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	gitT(t, dir, "worktree", "add", "-q", "-b", "feat", wt)
	tw := ta.otherApp(wt)

	ri, err := ta.repo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	riw, err := tw.repo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ri.repo != riw.repo {
		t.Fatalf("the worktree has another repo key: %s vs %s", riw.repo, ri.repo)
	}
	if ri.gitDir == riw.gitDir {
		t.Fatal("both worktrees share the git dir: the .env would be shared")
	}

	// El hook en el worktree: la copia de feat sale por fork de la de main, y la
	// de main NO se congela, porque el árbol principal la tiene activa.
	if err := tw.branchSwitch(ctx, "", defaultOwner, "", false); err != nil {
		t.Fatal(err)
	}
	mainC, feat := ta.copyOf(t, "main"), ta.copyOf(t, "feat")
	if feat == nil {
		t.Fatal("the main tree does not see the worktree's copy")
	}
	if mainC.State != api.StateRunning {
		t.Errorf("main was frozen while the main tree has it checked out: %s", mainC.State)
	}
	if _, err := os.Stat(filepath.Join(riw.gitDir, branchEnvFile)); err != nil {
		t.Errorf("no .env in the worktree's git dir: %v", err)
	}
	if s := gitT(t, wt, "status", "--porcelain", "--untracked-files=all"); s != "" {
		t.Errorf("worktree is dirty: %q", s)
	}

	// Desde el árbol principal, la misma rama es la misma copia: nada nuevo.
	n := len(ta.f.machines)
	if err := ta.branch(ctx, "feat", "", "", defaultOwner); err != nil {
		t.Fatal(err)
	}
	if len(ta.f.machines) != n {
		t.Errorf("the main tree created another copy of feat (%d -> %d machines)", n, len(ta.f.machines))
	}

	// Una rama que ningún worktree tiene activa sí se congela.
	gitT(t, dir, "branch", "idle")
	if err := ta.branch(ctx, "idle", "", "", defaultOwner); err != nil {
		t.Fatal(err)
	}
	if err := tw.branchSwitch(ctx, "", defaultOwner, "", false); err != nil {
		t.Fatal(err)
	}
	if st := ta.copyOf(t, "idle").State; st != api.StateWarm {
		t.Errorf("idle is %s, want frozen", st)
	}
	if ta.copyOf(t, "main").State != api.StateRunning || ta.copyOf(t, "feat").State != api.StateRunning {
		t.Error("a branch checked out in a worktree was frozen")
	}
}

func TestBranchClaveAntigua(t *testing.T) {
	ta, _ := branchTestApp(t)
	ctx := context.Background()
	ri, err := ta.repo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ri.legacy == ri.repo {
		t.Fatal("the new key must differ from the old one")
	}
	// Una copia de antes (kling.db.repo = hash del toplevel) sigue siendo la de
	// la rama: no se crea otra.
	old := ta.f.newMachine("db-old", map[string]string{labelGolden: "pg", labelOwner: defaultOwner, labelState: stateReady,
		labelRepo: ri.legacy, labelBranch: branchKey("main"), api.LabelKind: api.KindSandbox})
	if err := dbstate.WritePassword(old.ID, "pw-antigua"); err != nil {
		t.Fatal(err)
	}
	n := len(ta.f.machines)
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	if len(ta.f.machines) != n || !strings.Contains(ta.out.String(), "db-old") {
		t.Fatalf("the old copy was not reused:\n%s", ta.out)
	}
}

func TestBranchCerrojo(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "checkout", "-q", "-b", "feat")
	ri, _ := ta.repo(ctx)

	// Con el cerrojo ocupado (otro checkout en marcha), -switch espera y, si el
	// otro no acaba, se rinde sin crear nada.
	old := branchLockWait
	branchLockWait = 150 * time.Millisecond
	t.Cleanup(func() { branchLockWait = old })
	un, err := dbstate.Lock(ctx, "branch-"+ri.repo, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := len(ta.f.machines)
	err = ta.branchSwitch(ctx, "", defaultOwner, "", false)
	un()
	if err == nil || !strings.Contains(err.Error(), "another kling db branch") {
		t.Fatalf("err = %v", err)
	}
	if len(ta.f.machines) != n || !strings.Contains(ta.err.String(), "waiting") {
		t.Fatalf("created something or did not say it waited: %d machines\n%s", len(ta.f.machines), ta.err)
	}

	// Tres checkouts a la vez de una rama nueva: una sola copia.
	gitT(t, dir, "checkout", "-q", "-b", "feat2")
	branchLockWait = 10 * time.Second
	apps := []*testApp{ta.otherApp(dir), ta.otherApp(dir), ta.otherApp(dir)}
	var wg sync.WaitGroup
	errs := make([]error, len(apps))
	for i, x := range apps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = x.branchSwitch(ctx, "", defaultOwner, "", false)
		}()
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("switch %d: %v", i, e)
		}
	}
	copies, err := ta.repoCopies(ctx, ri, defaultOwner)
	if err != nil {
		t.Fatal(err)
	}
	if l := copies[branchKey("feat2")]; len(l) != 1 {
		t.Fatalf("%d copies of feat2", len(l))
	}
}

func TestHookConDuenoYGolden(t *testing.T) {
	ta, dir := branchTestApp(t)
	o := hookOpts{owner: "team-a", golden: "crm.v2"}
	if err := ta.branchHook(context.Background(), "install", o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ta.out.String(), "-switch -owner 'team-a' -golden 'crm.v2'") {
		t.Errorf("install output:\n%s", ta.out)
	}
	klog := filepath.Join(dir, "kling.log")
	t.Setenv("KLING", writeStub(t, dir, "fakekling", "printf '%s|' \"$@\" >> '"+klog+"'; echo >> '"+klog+"'\n"))
	gitT(t, dir, "checkout", "-q", "-b", "nb")
	if b, _ := os.ReadFile(klog); string(b) != "db|branch|-switch|-owner|team-a|-golden|crm.v2|\n" {
		t.Errorf("kling calls: %q", b)
	}
	// Reinstalar sin opciones deja el hook de siempre (idempotente, sin chain).
	if err := ta.branchHook(context.Background(), "install", hookOpts{}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, ".git", "hooks", hookName))
	if strings.Contains(string(b), "-owner") || string(b) != hookScript(hookOpts{}) {
		t.Errorf("reinstall kept the old flags:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "hooks", hookOrigName)); err == nil {
		t.Error("reinstall chained our own hook")
	}
}

func TestHookUsoConFlags(t *testing.T) {
	for _, args := range [][]string{
		{"-golden", "pg", "hook", "uninstall"},
		{"-ls", "hook", "install"},
		{"-from", "main", "hook", "install"},
		{"-ls", "-golden", "pg"},
		{"-rm", "x", "-golden", "pg"},
	} {
		err := cmdBranch(args)
		if err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"-owner", "BAD OWNER", "hook", "install"},
		{"-golden", "../x", "hook", "install"},
	} {
		if err := cmdBranch(args); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}
