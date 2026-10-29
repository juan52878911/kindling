package machine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Un exec que llega con la máquina a medio congelar (idle_freeze) se quedaba
// con la foto "running", esperaba 30 s a un agente que ya no estaba y daba
// 504. ExecTarget espera ahora a que el freeze suelte el cerrojo y decide con
// el estado de después. Aquí el "freeze" la deja parada, para no necesitar un
// VMM: lo que se comprueba es que no vuelve antes y que no vuelve con la foto
// vieja.
func TestExecTargetEsperaAUnFreezeEnCurso(t *testing.T) {
	m := newTestManager(t)
	id := "eeee000000000001"
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.AllowExec = true
	m.mu.Unlock()

	soltar := m.lock(id)
	quitar := m.marcarTransicion(id, api.TransitionFreezing)
	type res struct {
		mc  *api.Machine
		err error
	}
	hecho := make(chan res, 1)
	go func() {
		mc, err := m.ExecTarget(context.Background(), id)
		hecho <- res{mc, err}
	}()
	select {
	case r := <-hecho:
		t.Fatalf("ExecTarget volvió con el freeze en curso: %+v, %v", r.mc, r.err)
	case <-time.After(100 * time.Millisecond):
	}
	m.mu.Lock()
	mc.State = api.StateStopped
	m.mu.Unlock()
	quitar()
	soltar()

	select {
	case r := <-hecho:
		if !errors.Is(r.err, ErrNotRunning) {
			t.Fatalf("ExecTarget = %+v, %v; quería ErrNotRunning (el estado de después del freeze)", r.mc, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ExecTarget no volvió al soltar el cerrojo")
	}
}

// Sin transición, ExecTarget no toca el cerrojo: un exec no espera a otra
// operación larga de la máquina (un commit, un resize) que no la congela.
func TestExecTargetSinTransicionNoEsperaAlCerrojo(t *testing.T) {
	m := newTestManager(t)
	id := "eeee000000000002"
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.AllowExec = true
	m.mu.Unlock()
	defer m.lock(id)()

	hecho := make(chan error, 1)
	go func() {
		_, err := m.ExecTarget(context.Background(), id)
		hecho <- err
	}()
	select {
	case err := <-hecho:
		// Sin red de prueba no es alcanzable: lo que importa es que volvió.
		_ = err
	case <-time.After(2 * time.Second):
		t.Fatal("ExecTarget esperó al cerrojo sin un freeze en curso")
	}
}
