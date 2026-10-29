package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/plugin"
)

// Los códigos de salida son contrato (docs/guide.md, "Exit codes"): 0 bien,
// 1 error, 2 uso, 3 daemon incompatible, 4 dorado obsoleto, 5 al agente le
// falta una capacidad, 130 interrumpido; exec/shell/try salen con el remoto.
func TestCodigosDeSalida(t *testing.T) {
	for _, c := range []struct {
		err    error
		quiero int
	}{
		{nil, 0},
		{errors.New("boom"), salidaError},
		{fmt.Errorf("usage: kling start <ref>..."), salidaUso},
		{&errConCodigo{code: 7, err: errors.New("x")}, 7},
		{fmt.Errorf("get: %w", &api.ErrDaemonTooOld{Version: "0.17.0", API: 0}), salidaDaemonViejo},
		{&api.StatusError{Code: 409, Message: "x", ErrorCode: api.CodeStaleGolden}, salidaDoradoObsoleto},
		{&api.StatusError{Code: 409, Message: "stale golden snapshot: made with vz"}, salidaDoradoObsoleto},
		{&api.StatusError{Code: 409, Message: "x", ErrorCode: api.CodeConflict}, salidaError},
		{&errAgenteSinCapacidad{maquina: "m", faltan: []string{"env"}}, salidaAgenteSinCap},
		{&plugin.ExitError{Code: 9}, 9},
		{removedAliasError("rmi"), salidaUso},
	} {
		if got := codigoDeSalida(c.err); got != c.quiero {
			t.Errorf("%v: %d, quería %d", c.err, got, c.quiero)
		}
	}
	// Tras la señal, lo que falle es la interrupción.
	interrumpido.Store(true)
	defer interrumpido.Store(false)
	if got := codigoDeSalida(errors.New("context canceled")); got != salidaInterrumpido {
		t.Errorf("interrumpido: %d", got)
	}
	if got := codigoDeSalida(nil); got != 0 {
		t.Errorf("interrumpido sin error: %d", got)
	}
}

// agenteSinCapacidad sigue las reglas con las que el daemon falla la máquina:
// solo lo que la petición pide y el agente, que ya contestó, no anuncia.
func TestAgenteSinCapacidad(t *testing.T) {
	viejo := &api.GuestAgent{Version: "v0.17.0", Caps: []string{api.GuestCapExec}}
	nuevo := &api.GuestAgent{Version: "v0.20.0", Caps: []string{api.GuestCapEnv, api.GuestCapCmd, api.GuestCapVolumeWait}}
	for _, c := range []struct {
		req    api.RunRequest
		agente *api.GuestAgent
		falta  bool
	}{
		{api.RunRequest{Env: []string{"A=1"}}, viejo, true},
		{api.RunRequest{Cmd: []string{"sh"}}, viejo, true},
		{api.RunRequest{VolumeWait: true}, &api.GuestAgent{}, true},
		{api.RunRequest{Env: []string{"A=1"}, Cmd: []string{"sh"}, VolumeWait: true}, nuevo, false},
		{api.RunRequest{}, viejo, false},
		{api.RunRequest{Env: []string{"A=1"}}, nil, false}, // aún no contestó: no se sabe
	} {
		err := agenteSinCapacidad(c.req, &api.Machine{Name: "m", Agent: c.agente})
		if (err != nil) != c.falta {
			t.Errorf("%+v con %+v: %v", c.req, c.agente, err)
		}
		if err != nil && codigoDeSalida(err) != salidaAgenteSinCap {
			t.Errorf("código %d", codigoDeSalida(err))
		}
	}
}
