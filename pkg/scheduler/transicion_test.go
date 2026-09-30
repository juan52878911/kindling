package scheduler

import (
	"context"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Una instancia que el daemon está congelando (su TTL, un `kling freeze`, otro
// gateway) sigue figurando "running" hasta que termina el volcado. El
// planificador la adoptaba como "ya en marcha" y le enrutaba sesiones que
// morían con la pausa.
func TestNoAdoptaUnaMaquinaAMedioCongelar(t *testing.T) {
	mc := instanciaVieja("m1")
	mc.State = api.StateRunning
	mc.Transition = api.TransitionFreezing
	d := &daemonFalso{maquinas: map[string]*api.Machine{"m1": mc}}
	g := conDaemonFalso(t, d)

	got, how, err := g.acquire(context.Background(), "svc", false, nil)
	if err == nil && got != nil && got.ID == "m1" {
		t.Fatalf("acquire adoptó m1 (%s) a medio congelar", how)
	}
	if n := d.visto("POST /machines/m1/renew"); n != 0 {
		t.Errorf("renovó el TTL de una máquina a medio congelar (%d veces)", n)
	}
}
