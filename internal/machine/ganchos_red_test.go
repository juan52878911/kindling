package machine

import (
	"testing"
	"time"
)

// cambiarGanchosRed cambia los ganchos de red del host con cambiar (que
// recibe los vigentes) y los deja como estaban al acabar el test. Con el
// cerrojo de ganchosRedActuales: el trabajo de fondo de otro test puede estar
// leyéndolos.
func cambiarGanchosRed(t testing.TB, cambiar func(g *ganchosRed)) {
	t.Helper()
	ganchosRedMu.Lock()
	previos := ganchosRedV
	cambiar(&ganchosRedV)
	ganchosRedMu.Unlock()
	t.Cleanup(func() {
		ganchosRedMu.Lock()
		ganchosRedV = previos
		ganchosRedMu.Unlock()
	})
}

// esperarSueltas espera a lo que m dejó en segundo plano sin que Close lo
// espere (m.sueltas): un VMM de pega que muere al acabar el test no debe
// seguir trabajando en el siguiente.
func esperarSueltas(t testing.TB, m *Manager) {
	t.Helper()
	hecho := make(chan struct{})
	go func() { m.sueltas.Wait(); close(hecho) }()
	select {
	case <-hecho:
	case <-time.After(10 * time.Second):
		t.Error("trabajo de fondo del Manager sigue en marcha 10 s después del test")
	}
}
