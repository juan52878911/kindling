package main

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// reservaEnMarcha deja main con una reserva lista pero en marcha: lo que queda
// si quien la sacó murió entre el fork y el freeze.
func reservaEnMarcha(t *testing.T) (*testApp, *repoInfo, *api.Machine) {
	t.Helper()
	ta, dir := branchTestApp(t)
	ctx := context.Background()
	if err := ta.branch(ctx, "", "", "pg", defaultOwner); err != nil {
		t.Fatal(err)
	}
	ta.switchTo(t, dir, "a", true)
	sp := ta.sparesOf(t, "main")
	if len(sp) != 1 {
		t.Fatalf("%d spares", len(sp))
	}
	ta.f.mu.Lock()
	sp[0].State = api.StateRunning
	ta.f.mu.Unlock()
	ri, _ := ta.repo(ctx)
	return ta, ri, sp[0]
}

// Con main en marcha y la reserva al día, settle se la queda, pero congelada:
// una reserva en marcha es una microVM del tamaño del golden ocupando RAM.
func TestSettleSpareCongelaLaReservaEnMarcha(t *testing.T) {
	ta, ri, sp := reservaEnMarcha(t)
	ctx := context.Background()
	if _, err := ta.f.Thaw(ctx, ta.copyOf(t, "main").ID); err != nil {
		t.Fatal(err)
	}
	if err := ta.settleSpare(ctx, ri, defaultOwner); err != nil {
		t.Fatal(err)
	}
	if cur := ta.f.find(sp.ID); cur == nil || cur.State != api.StateWarm {
		t.Fatalf("spare gone or not frozen (exists %v)", cur != nil)
	}
}

// Igual con main congelado (su huella se mira al adoptarla, pero la reserva no
// tiene por qué seguir en marcha).
func TestSettleSpareCongelaLaReservaConElPadreCongelado(t *testing.T) {
	ta, ri, sp := reservaEnMarcha(t)
	ctx := context.Background()
	if st := ta.copyOf(t, "main").State; st != api.StateWarm {
		t.Fatalf("main is %s", st)
	}
	if err := ta.settleSpare(ctx, ri, defaultOwner); err != nil {
		t.Fatal(err)
	}
	if cur := ta.f.find(sp.ID); cur == nil || cur.State != api.StateWarm {
		t.Fatalf("spare gone or not frozen (exists %v)", cur != nil)
	}
}

// inspectFalla es un daemon que no sabe decir cómo está una máquina.
type inspectFalla struct {
	*fakeKling
	id string
}

func (k inspectFalla) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	if len(args) >= 2 && args[0] == "inspect" && args[1] == k.id {
		return nil, errors.New("daemon unavailable")
	}
	return k.fakeKling.Run(ctx, stdin, args...)
}

// Si no se puede leer la reserva, withSpare lo dice: tragárselo dejaba sin
// congelar (o sin borrar) una reserva que sigue ahí.
func TestWithSpareNoTragaElErrorDeInspect(t *testing.T) {
	ta, ri, sp := reservaEnMarcha(t)
	ctx := context.Background()
	ta.app.k = inspectFalla{ta.f, sp.ID}
	called := false
	err := ta.withSpare(ctx, ri, sp.ID, branchKey("main"), defaultOwner, func(*api.Machine) error { called = true; return nil })
	if err == nil || called {
		t.Fatalf("withSpare with a failing inspect: err %v, fn called %v", err, called)
	}
	// Una que ya no existe sí es "nada que hacer".
	ta.app.k = ta.f
	if _, err := ta.f.Run(ctx, nil, "rm", "-f", sp.ID); err != nil {
		t.Fatal(err)
	}
	if err := ta.withSpare(ctx, ri, sp.ID, branchKey("main"), defaultOwner, func(*api.Machine) error { called = true; return nil }); err != nil || called {
		t.Fatalf("withSpare on a removed spare: err %v, fn called %v", err, called)
	}
}
