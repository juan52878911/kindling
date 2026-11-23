package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
)

// holdRepoLock toma el cerrojo de kling db branch del repo de la prueba (lo
// que tendría un checkout en marcha) y acorta la espera de los demás.
func (ta *testApp) holdRepoLock(t *testing.T) func() {
	t.Helper()
	ctx := context.Background()
	ri, err := ta.repo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	old := branchLockWait
	branchLockWait = 150 * time.Millisecond
	t.Cleanup(func() { branchLockWait = old })
	un, err := dbstate.Lock(ctx, "branch-"+ri.repo, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	return un
}

// reset, undo y rm borran y recrean la copia de una rama: lo hacen con el
// cerrojo del repo, como cualquier otro que cree o borre copias de rama. Si no,
// un checkout en ese hueco crea otra copia de la misma rama.
func TestResetUndoRmDeUnaRamaToman(t *testing.T) {
	ta, _ := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	mc := ta.copyOf(t, "main")
	un := ta.holdRepoLock(t)
	defer un()

	if _, err := ta.reset(ctx, mc.Name, defaultOwner); err == nil || !strings.Contains(err.Error(), "another kling db branch") {
		t.Errorf("reset with the repo lock taken: err = %v", err)
	}
	if _, err := ta.undo(ctx, mc.Name, "", defaultOwner); err == nil || !strings.Contains(err.Error(), "another kling db branch") {
		t.Errorf("undo with the repo lock taken: err = %v", err)
	}
	if err := ta.rmOne(ctx, mc.Name, defaultOwner); err == nil || !strings.Contains(err.Error(), "another kling db branch") {
		t.Errorf("rm with the repo lock taken: err = %v", err)
	}
	if ta.f.find(mc.ID) == nil {
		t.Fatal("the copy of the branch was removed while another branch operation held the lock")
	}
}

// Una copia que no es de ninguna rama no espera al cerrojo de nadie.
func TestResetDeUnaCopiaSueltaNoEspera(t *testing.T) {
	ta, _ := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	loose, err := ta.up(ctx, "pg", "suelta", 0, defaultOwner)
	if err != nil {
		t.Fatal(err)
	}
	un := ta.holdRepoLock(t)
	defer un()
	if _, err := ta.reset(ctx, loose.Name, defaultOwner); err != nil {
		t.Fatalf("reset of a copy with no branch: %v", err)
	}
}

// kling db fork de la copia de una rama da copias sueltas: si heredasen el
// repo y la rama habría dos copias de la misma rama, creadas sin el cerrojo.
func TestForkDeUnaRamaNoDuplicaLaRama(t *testing.T) {
	ta, _ := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	mc := ta.copyOf(t, "main")
	cs, err := ta.fork(ctx, mc.Name, 2, defaultOwner)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if l := ta.f.find(c.ID).Labels; l[labelBranch] != "" || l[labelRepo] != "" {
			t.Errorf("fork %s kept the branch labels: %v", c.Name, l)
		}
	}
	ri, _ := ta.repo(ctx)
	copies, err := ta.repoCopies(ctx, ri, defaultOwner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := one(copies, branchKey("main")); err != nil {
		t.Fatal(err)
	}
}

// Una copia de rama que se quedó en preparing (su creador murió entre el fork y
// marcarla lista) es huérfana: quien crea copias de rama lo hace con el
// cerrojo del repo, y aquí lo tiene quien la encuentra. Se tira y se rehace, en
// vez de bloquear la rama hasta un -rm a mano.
func TestBranchHuerfanaEnPreparingSeRehace(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	main := ta.copyOf(t, "main")
	gitT(t, dir, "checkout", "-q", "-b", "feat")
	l := clone(main.Labels)
	l[labelBranch] = branchKey("feat")
	l[labelState] = statePreparing
	ta.f.mu.Lock()
	orphan := ta.f.newMachine("huerfana", l)
	ta.f.mu.Unlock()

	if err := ta.branchSwitch(ctx, "", defaultOwner, "", true); err != nil {
		t.Fatalf("switch with an orphan copy in preparing: %v\n%s", err, ta.err)
	}
	if ta.f.find(orphan.ID) != nil {
		t.Error("the orphan copy is still there")
	}
	mc := ta.copyOf(t, "feat")
	if mc == nil || mc.ID == orphan.ID || mc.Labels[labelState] != stateReady {
		t.Fatalf("copy of feat = %+v", mc)
	}
}

// avisoEspera es un stderr que avisa (una vez) cuando alguien dice que espera
// al cerrojo.
type avisoEspera struct {
	mu   sync.Mutex
	buf  strings.Builder
	once sync.Once
	ch   chan struct{}
}

func (w *avisoEspera) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if strings.Contains(w.buf.String(), "waiting for another kling db branch") {
		w.once.Do(func() { close(w.ch) })
	}
	return len(p), nil
}

// -prune que espera el cerrojo detrás de un checkout que crea una rama nueva
// no borra la copia de esa rama: las ramas de git se leen con el cerrojo ya
// tomado, a la vez que las copias.
func TestPruneLeeLasRamasConElCerrojo(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	main := ta.copyOf(t, "main")
	ri, _ := ta.repo(ctx)
	old := branchLockWait
	branchLockWait = 10 * time.Second
	t.Cleanup(func() { branchLockWait = old })
	un, err := dbstate.Lock(ctx, "branch-"+ri.repo, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := &avisoEspera{ch: make(chan struct{})}
	ta.app.stderr = w
	done := make(chan error, 1)
	go func() { done <- ta.branchPrune(ctx, defaultOwner, false) }()
	select {
	case <-w.ch:
	case err := <-done:
		un()
		t.Fatalf("prune did not wait for the lock: %v", err)
	case <-time.After(5 * time.Second):
		un()
		t.Fatal("prune neither waited nor finished")
	}
	// Lo que hace el checkout que tiene el cerrojo: la rama y su copia.
	gitT(t, dir, "branch", "nueva")
	l := clone(main.Labels)
	l[labelBranch] = branchKey("nueva")
	ta.f.mu.Lock()
	nueva := ta.f.newMachine("nueva", l)
	ta.f.mu.Unlock()
	un()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if ta.f.find(nueva.ID) == nil {
		t.Fatal("prune removed the copy of a branch created while it waited for the lock")
	}
}

// Otro estado que no es ni ready ni preparing no se toca: no se sabe de dónde
// sale.
func TestBranchCopiaEnEstadoRaroNoSeTira(t *testing.T) {
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	main := ta.copyOf(t, "main")
	gitT(t, dir, "checkout", "-q", "-b", "feat")
	l := clone(main.Labels)
	l[labelBranch] = branchKey("feat")
	l[labelState] = "otro"
	ta.f.mu.Lock()
	odd := ta.f.newMachine("rara", l)
	ta.f.mu.Unlock()
	if err := ta.branchSwitch(ctx, "", defaultOwner, "", true); err == nil || !strings.Contains(err.Error(), "is not ready") {
		t.Fatalf("err = %v", err)
	}
	if ta.f.find(odd.ID) == nil {
		t.Fatal("a copy in an unknown state was removed")
	}
}
