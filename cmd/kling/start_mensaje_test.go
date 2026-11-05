package main

import (
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// "cold boot from its disk" solo cuando arrancó de verdad: a una que ya
// corría el daemon la devuelve con el StartedAt de antes.
func TestArrancoAhora(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	t1 := time.Now()
	corriendo := &api.Machine{State: api.StateRunning, StartedAt: &t0}
	casos := []struct {
		nombre  string
		antes   *api.Machine
		despues *api.Machine
		quiero  bool
	}{
		{"ya corría", corriendo, &api.Machine{State: api.StateRunning, StartedAt: &t0}, false},
		{"corría muerta y arrancó", corriendo, &api.Machine{State: api.StateRunning, StartedAt: &t1}, true},
		{"parada", &api.Machine{State: api.StateStopped, StartedAt: &t0}, &api.Machine{State: api.StateRunning, StartedAt: &t1}, true},
		{"failed", &api.Machine{State: api.StateFailed, StartedAt: &t0}, &api.Machine{State: api.StateRunning, StartedAt: &t1}, true},
		{"sin antes", nil, &api.Machine{State: api.StateRunning, StartedAt: &t1}, true},
	}
	for _, c := range casos {
		if got := arrancoAhora(c.antes, c.despues); got != c.quiero {
			t.Errorf("%s: arrancoAhora = %v; quería %v", c.nombre, got, c.quiero)
		}
	}
}
