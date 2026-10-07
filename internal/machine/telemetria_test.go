package machine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Lo que cuenta como fallo y lo que no. Un rechazo de admisión es la
// respuesta correcta de un host lleno; "no existe" o "está parada" es un error
// de quien llama. Ninguno de los dos dice que el daemon falle.
func TestTelemetriaClasificaLosErrores(t *testing.T) {
	antes := (&Manager{}).Telemetria()
	tel.fin(OpThaw, nil)
	tel.fin(OpThaw, noExiste("x"))
	tel.fin(OpThaw, fmt.Errorf("thawing: %w", estadoInvalido("is %s", "stopped")))
	tel.fin(OpFreeze, errYaNoToca)
	tel.fin(OpRun, &api.StatusError{Code: api.StatusDiskFull, Message: "lleno"})
	tel.fin(OpRun, fmt.Errorf("admisión: %w", &api.StatusError{Code: api.StatusInsufficientMemory}))
	tel.fin(OpRun, &api.StatusError{Code: 500})
	tel.fin(OpThaw, errors.New("resuming: EOF"))
	d := (&Manager{}).Telemetria()

	if n := d.Fallos[OpThaw] - antes.Fallos[OpThaw]; n != 1 {
		t.Errorf("fallos de thaw = %d, quería 1 (solo el EOF)", n)
	}
	if n := d.Fallos[OpFreeze] - antes.Fallos[OpFreeze]; n != 0 {
		t.Errorf("errYaNoToca contó como fallo")
	}
	if n := d.Fallos[OpRun] - antes.Fallos[OpRun]; n != 1 {
		t.Errorf("fallos de run = %d, quería 1 (el 500)", n)
	}
	if n := d.Rechazos[api.StatusDiskFull] - antes.Rechazos[api.StatusDiskFull]; n != 1 {
		t.Errorf("rechazos 503 = %d", n)
	}
	if n := d.Rechazos[api.StatusInsufficientMemory] - antes.Rechazos[api.StatusInsufficientMemory]; n != 1 {
		t.Errorf("rechazos 507 (envuelto) = %d", n)
	}
}

func TestHistogramaCubos(t *testing.T) {
	var h histograma
	for _, ms := range []int64{0, 5, 6, 99999} {
		h.observar(ms)
	}
	if h.cubos[0].Load() != 2 || h.cubos[1].Load() != 1 || h.cubos[len(LimitesDuracionMS)].Load() != 1 {
		t.Fatalf("cubos mal repartidos: [0]=%d [1]=%d [inf]=%d", h.cubos[0].Load(), h.cubos[1].Load(), h.cubos[len(LimitesDuracionMS)].Load())
	}
	if h.cuenta.Load() != 4 || h.suma.Load() != 0+5+6+99999 {
		t.Fatalf("cuenta %d suma %d", h.cuenta.Load(), h.suma.Load())
	}
}

// El tope de máquinas es un rechazo y se cuenta como tal al pasar por Run.
func TestRunCuentaElRechazoPorTope(t *testing.T) {
	t.Setenv("KLING_MAX_MACHINES", "1")
	m := newTestManager(t)
	m.addForTest(newID())
	antes := m.Telemetria()
	_, err := m.Run(context.Background(), api.RunRequest{Image: "min"})
	if !api.IsMachineLimit(err) {
		t.Fatalf("quería el rechazo por tope, salió %v", err)
	}
	d := m.Telemetria()
	if n := d.Rechazos[api.StatusMachineLimit] - antes.Rechazos[api.StatusMachineLimit]; n != 1 {
		t.Errorf("rechazos 409 = %d, quería 1", n)
	}
	if d.Fallos[OpRun] != antes.Fallos[OpRun] {
		t.Error("un rechazo contó como fallo")
	}
}

func TestTelemetriaDiscoYPendiente(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	m.pendingMiB = 321
	m.mu.Unlock()
	d := m.Telemetria()
	if d.PendienteMiB != 321 {
		t.Errorf("PendienteMiB = %d", d.PendienteMiB)
	}
	if d.DiscoLibreMiB <= 0 {
		t.Errorf("DiscoLibreMiB = %d bajo un directorio temporal", d.DiscoLibreMiB)
	}
}

// El histograma de thaw y resume mide el despertar entero, no solo el
// LoadSnapshot: en el lab ThawMS era 4-6 ms de un despertar de 110-300.
func TestDespertarMideElTotal(t *testing.T) {
	antes := tel.dur[DurThaw].suma.Load()
	tel.despertar(DurThaw, &api.WakePhases{LoadMS: 5, TotalMS: 180})
	if d := tel.dur[DurThaw].suma.Load() - antes; d != 180 {
		t.Fatalf("thaw anotó %d ms, quería el total (180)", d)
	}
}
