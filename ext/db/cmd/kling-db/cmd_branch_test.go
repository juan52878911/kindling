package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// gitT ejecuta git en dir (el repo temporal de la prueba).
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepo crea un repo git real con un commit en main.
func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", "a.txt")
	gitT(t, dir, "commit", "-q", "-m", "one")
	return dir
}

func branchTestApp(t *testing.T) (*testApp, string) {
	t.Helper()
	dir := newRepo(t)
	ta := newTestApp(t)
	ta.cwd = dir
	return ta, dir
}

func (ta *testApp) copyOf(t *testing.T, branch string) *api.Machine {
	t.Helper()
	ri, err := ta.repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	copies, err := ta.repoCopies(context.Background(), ri, defaultOwner)
	if err != nil {
		t.Fatal(err)
	}
	mc, err := one(copies, branchKey(branch))
	if err != nil {
		t.Fatal(err)
	}
	if mc == nil {
		return nil
	}
	return ta.f.find(mc.ID) // la viva: ps devuelve copias del JSON
}

func TestNombresDeRama(t *testing.T) {
	for _, b := range []string{"", "-x", "a\nb", "a\x00", "a\u0085b", "a‮b", "a‏b", "a⁦b", strings.Repeat("a", 300), "\xff"} {
		if validBranch(b) == nil {
			t.Errorf("validBranch(%q) accepted", b)
		}
	}
	seen := map[string]string{}
	for _, b := range []string{"main", "feat/x", "feat-x", "Feat/X", "feat_x", "ñandú", "日本", strings.Repeat("a/", 100), "release/1.2.3"} {
		if err := validBranch(b); err != nil {
			t.Fatalf("%q: %v", b, err)
		}
		k := branchKey(b)
		if !api.KeyPattern.MatchString(k) || !namePattern.MatchString(branchMachineName("abcdef012345", k)) {
			t.Errorf("key %q of %q is not a valid label/name", k, b)
		}
		if k != branchKey(b) {
			t.Errorf("key of %q is not stable", b)
		}
		if o, dup := seen[k]; dup {
			t.Errorf("%q and %q share key %q", b, o, k)
		}
		seen[k] = b
	}
}

func TestBranchCreaDelGoldenYEsIdempotente(t *testing.T) {
	ta, _ := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	mc := ta.copyOf(t, "main")
	if mc == nil {
		t.Fatal("no copy for main")
	}
	ri, _ := ta.repo(ctx)
	if mc.Labels[labelRepo] != ri.repo || mc.Labels[labelBranch] != branchKey("main") || mc.Labels[labelState] != stateReady {
		t.Fatalf("labels: %v", mc.Labels)
	}
	if mc.Name != branchMachineName(ri.repo, branchKey("main")) {
		t.Errorf("name %q", mc.Name)
	}
	if !strings.Contains(ta.out.String(), "kling db connect "+mc.Name) {
		t.Errorf("no connection hint:\n%s", ta.out)
	}
	// Es la rama actual: la conexión ya está en .git, sin esperar al hook.
	fi, err := os.Stat(filepath.Join(ri.gitDir, branchEnvFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("branch on the current branch must write .git/%s 0600: %v %v", branchEnvFile, fi, err)
	}
	n := len(ta.f.machines)
	ta.out.Reset()
	if err := ta.branch(ctx, "", "", "", defaultOwner); err != nil {
		t.Fatal(err)
	}
	if len(ta.f.machines) != n || !strings.Contains(ta.out.String(), "ready") {
		t.Errorf("second call created something: %d machines\n%s", len(ta.f.machines), ta.out)
	}
}

func TestBranchSinPadreNiGoldenFalla(t *testing.T) {
	ta, _ := branchTestApp(t)
	err := ta.branch(context.Background(), "", "", "", defaultOwner)
	if err == nil || !strings.Contains(err.Error(), "-golden") {
		t.Fatalf("err = %v", err)
	}
	if len(ta.f.machines) != 0 {
		t.Error("created something")
	}
}

func TestBranchForkDelPadre(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	parent := ta.copyOf(t, "main")
	gitT(t, dir, "checkout", "-q", "-b", "feat/x")
	// La rama padre está congelada: se ramifica y vuelve a congelarse.
	parent.State = api.StateWarm
	ta.f.calls = nil
	if err := ta.branch(ctx, "", "", "", defaultOwner); err != nil {
		t.Fatal(err)
	}
	child := ta.copyOf(t, "feat/x")
	if child == nil || child.ID == parent.ID {
		t.Fatalf("no child copy: %+v", child)
	}
	if child.Labels[labelBranch] != branchKey("feat/x") || child.Labels[labelRepo] != parent.Labels[labelRepo] {
		t.Errorf("child labels %v", child.Labels)
	}
	if parent.State != api.StateWarm {
		t.Errorf("parent is %s, want frozen again", parent.State)
	}
	forked, ran := false, false
	for _, c := range ta.f.calls {
		if len(c.args) > 1 && c.args[0] == "sandbox" && c.args[1] == "fork" {
			forked = true
			joined := strings.Join(c.args, " ")
			if !strings.Contains(joined, "-label "+labelBranch+"="+branchKey("feat/x")) {
				t.Errorf("fork without the branch label: %v", c.args)
			}
		}
		if len(c.args) > 0 && c.args[0] == "run" {
			ran = true
		}
	}
	if !forked || ran {
		t.Errorf("forked=%v ran=%v", forked, ran)
	}
	// La copia hija tiene clave propia en el host.
	if password(t, child.ID) == password(t, parent.ID) {
		t.Error("child shares the parent's password")
	}
}

func TestBranchFromExplicito(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "dev", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "checkout", "-q", "-b", "topic")
	ta.f.calls = nil
	if err := ta.branch(ctx, "", "dev", "", defaultOwner); err != nil {
		t.Fatal(err)
	}
	for _, c := range ta.f.calls {
		if len(c.args) > 0 && c.args[0] == "run" {
			t.Fatalf("started from the golden: %v", c.args)
		}
	}
	if ta.copyOf(t, "topic") == nil {
		t.Fatal("no copy")
	}
}

func TestBranchHEADSuelto(t *testing.T) {
	ta, dir := branchTestApp(t)
	gitT(t, dir, "checkout", "-q", "--detach")
	err := ta.branch(context.Background(), "", "", "pg", defaultOwner)
	if err == nil || !strings.Contains(err.Error(), "detached") {
		t.Fatalf("err = %v", err)
	}
}

func TestBranchFueraDeGit(t *testing.T) {
	ta := newTestApp(t)
	ta.cwd = t.TempDir()
	if err := ta.branch(context.Background(), "", "", "pg", defaultOwner); err == nil {
		t.Fatal("outside a repo it worked")
	}
}

func TestBranchSwitch(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "checkout", "-q", "-b", "feat/x")
	if err := ta.branchSwitch(ctx, "", defaultOwner, "", false); err != nil {
		t.Fatal(err)
	}
	mainC, feat := ta.copyOf(t, "main"), ta.copyOf(t, "feat/x")
	if mainC.State != api.StateWarm || feat.State != api.StateRunning {
		t.Fatalf("main=%s feat=%s", mainC.State, feat.State)
	}
	envPath := filepath.Join(dir, ".git", branchEnvFile)
	st, err := os.Stat(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("env mode %v", st.Mode().Perm())
	}
	b, _ := os.ReadFile(envPath)
	if !strings.Contains(string(b), "DATABASE_URL=postgres://app:"+password(t, feat.ID)+"@") || strings.Contains(string(b), password(t, mainC.ID)) {
		t.Errorf("env:\n%s", b)
	}
	// Nada de la conexión en el árbol de trabajo.
	if s := gitT(t, dir, "status", "--porcelain", "--untracked-files=all"); s != "" {
		t.Errorf("working tree is dirty: %q", s)
	}
	if strings.Contains(ta.err.String(), password(t, feat.ID)) || strings.Contains(ta.out.String(), password(t, feat.ID)) {
		t.Error("the password was printed")
	}
	// Vuelta a main: se descongela y la otra se congela; el .env cambia.
	gitT(t, dir, "checkout", "-q", "main")
	if err := ta.branchSwitch(ctx, "", defaultOwner, "", false); err != nil {
		t.Fatal(err)
	}
	if mainC.State != api.StateRunning || feat.State != api.StateWarm {
		t.Fatalf("main=%s feat=%s", mainC.State, feat.State)
	}
	b, _ = os.ReadFile(envPath)
	if !strings.Contains(string(b), password(t, mainC.ID)) || strings.Contains(string(b), password(t, feat.ID)) {
		t.Errorf("env not switched:\n%s", b)
	}
}

func TestBranchSwitchFallaSinEnvViejo(t *testing.T) {
	ta, dir := branchTestApp(t)
	envPath := filepath.Join(dir, ".git", branchEnvFile)
	if err := os.WriteFile(envPath, []byte("DATABASE_URL=stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Sin copia, sin padre y sin golden: falla, y el .env de otra rama ya no está.
	if err := ta.branchSwitch(context.Background(), "", defaultOwner, "", false); err == nil {
		t.Fatal("no error")
	}
	if _, err := os.Stat(envPath); err == nil {
		t.Error("stale env kept")
	}
}

func TestBranchSwitchUsaElGoldenDelRepo(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	ta.copyOf(t, "main").Labels[labelState] = statePreparing // el padre no sirve
	gitT(t, dir, "checkout", "-q", "-b", "other")
	if err := ta.branchSwitch(ctx, "", defaultOwner, "", false); err != nil {
		t.Fatal(err)
	}
	if mc := ta.copyOf(t, "other"); mc == nil || mc.From != "pg" {
		t.Fatalf("copy: %+v", mc)
	}
}

func TestBranchSwitchAvisaSiNoCongela(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "checkout", "-q", "-b", "b2")
	ta.f.freezeFails = true
	err := ta.branchSwitch(ctx, "", defaultOwner, "", false)
	if err == nil || !strings.Contains(err.Error(), "could not freeze") {
		t.Fatalf("err = %v", err)
	}
	// La rama activa ya está lista y con su .env.
	if _, e := os.Stat(filepath.Join(dir, ".git", branchEnvFile)); e != nil {
		t.Error(e)
	}
}

func TestBranchLsRmPrune(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "branch", "gone")
	gitT(t, dir, "branch", "keep")
	for _, b := range []string{"gone", "keep"} {
		if err := ta.branch(ctx, b, "", "", defaultOwner); err != nil {
			t.Fatal(err)
		}
	}
	// Una copia de OTRO dueño no se lista ni se toca.
	other := ta.f.newMachine("foreign", map[string]string{labelGolden: "pg", labelOwner: "otro", labelState: stateReady,
		labelRepo: ta.copyOf(t, "main").Labels[labelRepo], labelBranch: branchKey("gone")})
	ta.out.Reset()
	if err := ta.branchLs(ctx, defaultOwner, false); err != nil {
		t.Fatal(err)
	}
	o := ta.out.String()
	for _, w := range []string{"* main", "gone", "keep", "BRANCH", "running"} {
		if !strings.Contains(o, w) {
			t.Errorf("ls lacks %q:\n%s", w, o)
		}
	}
	if strings.Contains(o, "foreign") {
		t.Errorf("ls shows another owner's copy:\n%s", o)
	}
	ta.out.Reset()
	if err := ta.branchLs(ctx, defaultOwner, true); err != nil || !strings.Contains(ta.out.String(), `"branch":"keep"`) {
		t.Fatalf("json: %v %s", err, ta.out)
	}

	gitT(t, dir, "branch", "-D", "gone")
	goneC := ta.copyOf(t, "gone")
	ta.out.Reset()
	if err := ta.branchPrune(ctx, defaultOwner, true); err != nil {
		t.Fatal(err)
	}
	if ta.f.find(goneC.ID) == nil || !strings.Contains(ta.out.String(), "would remove") {
		t.Fatal("dry-run removed or said nothing")
	}
	if err := ta.branchPrune(ctx, defaultOwner, false); err != nil {
		t.Fatal(err)
	}
	if ta.f.find(goneC.ID) != nil || ta.copyOf(t, "keep") == nil || ta.copyOf(t, "main") == nil {
		t.Fatal("prune removed the wrong ones")
	}
	if ta.f.find(other.ID) == nil {
		t.Fatal("prune removed a foreign copy")
	}
	if _, err := os.Stat(passwordFile(goneC.ID)); err == nil {
		t.Error("the password of the pruned copy is still on the host")
	}

	keepC := ta.copyOf(t, "keep")
	if err := ta.branchRm(ctx, "keep", defaultOwner); err != nil {
		t.Fatal(err)
	}
	if ta.f.find(keepC.ID) != nil {
		t.Error("rm kept the machine")
	}
	if err := ta.branchRm(ctx, "keep", defaultOwner); err == nil {
		t.Error("rm of a branch without copy worked")
	}
	if err := ta.branchRm(ctx, "-x", defaultOwner); err == nil {
		t.Error("rm accepted an invalid name")
	}
}

func passwordFile(id string) string {
	return filepath.Join(os.Getenv("KLING_DB_STATE"), "copies", id, "password")
}

func TestBranchUso(t *testing.T) {
	for _, args := range [][]string{
		{"-switch", "-ls"},
		{"-ls", "x"},
		{"a", "b"},
		{"-dry-run"},
		{"-json"},
		{"-switch", "-from", "x"},
		{"hook", "bogus"},
		{"-rm", "a", "-prune"},
	} {
		err := cmdBranch(args)
		if err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}

// ── hook ─────────────────────────────────────────────────────────────────────

func writeStub(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHookInstalaYDesinstala(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	hook := filepath.Join(dir, ".git", "hooks", hookName)

	if err := ta.branchHook(ctx, "install", hookOpts{}); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(hook)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(hook); st.Mode().Perm()&0o100 == 0 {
		t.Error("hook not executable")
	}
	if err := ta.branchHook(ctx, "install", hookOpts{}); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(hook)
	if string(first) != string(second) {
		t.Error("install is not idempotent")
	}
	if _, err := os.Stat(hook + ".pre-kling-db"); err == nil {
		t.Error("it chained itself")
	}
	if err := ta.branchHook(ctx, "uninstall", hookOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hook); err == nil {
		t.Error("hook still there")
	}
	// Desinstalar sin hook no falla.
	if err := ta.branchHook(ctx, "uninstall", hookOpts{}); err != nil {
		t.Fatal(err)
	}
}

func TestHookEncadenaUnoExistente(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	hooks := filepath.Join(dir, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(dir, "mine.log")
	writeStub(t, hooks, hookName, "echo old >> '"+mine+"'\n")
	if err := ta.branchHook(ctx, "install", hookOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(hooks, hookOrigName)); err != nil {
		t.Fatalf("the old hook was not kept: %v", err)
	}
	klog := filepath.Join(dir, "kling.log")
	t.Setenv("KLING", writeStub(t, dir, "fakekling", "echo \"$@\" >> '"+klog+"'\n"))
	gitT(t, dir, "checkout", "-q", "-b", "newbranch")
	if b, _ := os.ReadFile(mine); string(b) != "old\n" {
		t.Errorf("the previous hook did not run: %q", b)
	}
	if b, _ := os.ReadFile(klog); string(b) != "db branch -switch\n" {
		t.Errorf("kling calls: %q", b)
	}
	// Un checkout de ficheros (a.txt) no cambia de base.
	gitT(t, dir, "checkout", "-q", "--", "a.txt")
	if b, _ := os.ReadFile(klog); string(b) != "db branch -switch\n" {
		t.Errorf("file checkout called kling: %q", b)
	}
	if err := ta.branchHook(ctx, "uninstall", hookOpts{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(hooks, hookName)); !strings.Contains(string(b), "echo old") {
		t.Errorf("the old hook was not restored: %q", b)
	}
	if _, err := os.Stat(filepath.Join(hooks, hookOrigName)); err == nil {
		t.Error("the copy of the old hook is still there")
	}
}

func TestHookNoBloqueaElCheckout(t *testing.T) {
	ta, dir := branchTestApp(t)
	if err := ta.branchHook(context.Background(), "install", hookOpts{}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KLING", writeStub(t, dir, "badkling", "echo boom >&2\nexit 7\n"))
	gitT(t, dir, "checkout", "-q", "-b", "x") // gitT falla el test si git falla
	if got := gitT(t, dir, "symbolic-ref", "--short", "HEAD"); got != "x" {
		t.Errorf("HEAD = %s", got)
	}
	// Sin kling encontrado tampoco.
	t.Setenv("KLING", filepath.Join(dir, "nope"))
	gitT(t, dir, "checkout", "-q", "main")
}

func TestHookRechazaEnlaces(t *testing.T) {
	ta, dir := branchTestApp(t)
	hooks := filepath.Join(dir, ".git", "hooks")
	_ = os.MkdirAll(hooks, 0o755)
	target := filepath.Join(dir, "elsewhere")
	_ = os.WriteFile(target, []byte("x"), 0o755)
	if err := os.Symlink(target, filepath.Join(hooks, hookName)); err != nil {
		t.Skip(err)
	}
	if err := ta.branchHook(context.Background(), "install", hookOpts{}); err == nil {
		t.Fatal("it followed a symlink")
	}
	if b, _ := os.ReadFile(target); string(b) != "x" {
		t.Error("the link target was modified")
	}
}

func TestHookScriptSintaxis(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip()
	}
	p := filepath.Join(t.TempDir(), "h")
	_ = os.WriteFile(p, []byte(hookScript(hookOpts{})), 0o755)
	if out, err := exec.Command(sh, "-n", p).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
}
