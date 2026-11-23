package main

import (
	"context"
	"strings"
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
