package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// printReady sale con 1 si no está lista, para que un script pueda
// encadenarlo (`kling machine ready m && ...`), y con 0 si no hay nada que
// esperar.
func TestPrintReady(t *testing.T) {
	casos := []struct {
		nombre string
		res    api.ReadyResult
		codigo int
		dice   []string
	}{
		{"lista", api.ReadyResult{ID: "abcdef0123456789", Name: "m", Ready: api.ReadyYes,
			Guest: &api.GuestReady{Ready: true, Probe: true}, WaitedMS: 30},
			0, []string{"abcdef012345  ready  (waited 30 ms)", "probe  " + api.GuestReadyProbe}},
		{"agente viejo", api.ReadyResult{ID: "abcdef0123456789", Name: "m"},
			0, []string{"ready (no readiness probe"}},
		{"no declara", api.ReadyResult{ID: "abcdef0123456789", Name: "m", Guest: &api.GuestReady{Ready: true}},
			0, []string{"declares no probe nor hooks"}},
		{"esperando", api.ReadyResult{ID: "abcdef0123456789", Name: "m", Ready: api.ReadyWaiting,
			Guest: &api.GuestReady{Probe: true, Detail: "sys.boot_completed is not 1"}},
			1, []string{"waiting", "why    sys.boot_completed is not 1"}},
		{"ganchos fallidos", api.ReadyResult{ID: "abcdef0123456789", Name: "m", Ready: api.ReadyFailed,
			Guest: &api.GuestReady{HasHooks: true, Hooks: api.HooksFailed, Detail: "10-id: exit status 1"}},
			1, []string{"failed", "hooks  " + api.GuestPostRestoreDir + "/*  failed", "why    10-id"}},
		// El agente no contestó: el motivo viene del daemon, no del invitado.
		{"sin respuesta", api.ReadyResult{ID: "abcdef0123456789", Name: "m", Ready: api.ReadyWaiting,
			Detail: "guest answered 500 to /ready: boom"},
			1, []string{"waiting", "why    guest answered 500"}},
	}
	for _, c := range casos {
		var out bytes.Buffer
		err := printReady(&out, &c.res, false)
		if got := codigoSalidaDe(err); got != c.codigo {
			t.Errorf("%s: código %d (%v), quiero %d", c.nombre, got, err, c.codigo)
		}
		for _, d := range c.dice {
			if !strings.Contains(out.String(), d) {
				t.Errorf("%s: no dice %q:\n%s", c.nombre, d, out.String())
			}
		}
		if c.codigo == 0 && strings.Contains(out.String(), "waiting") {
			t.Errorf("%s: código 0 diciendo waiting:\n%s", c.nombre, out.String())
		}

		// Con -json, el mismo código y el resultado tal cual.
		out.Reset()
		err = printReady(&out, &c.res, true)
		var vuelta api.ReadyResult
		if json.Unmarshal(out.Bytes(), &vuelta) != nil || vuelta.Ready != c.res.Ready || vuelta.Detail != c.res.Detail {
			t.Errorf("%s: -json = %s", c.nombre, out.String())
		}
		if got := codigoSalidaDe(err); got != c.codigo {
			t.Errorf("%s -json: código %d, quiero %d", c.nombre, got, c.codigo)
		}
	}
}

func codigoSalidaDe(err error) int {
	if err == nil {
		return 0
	}
	return codigoDeSalida(err)
}
