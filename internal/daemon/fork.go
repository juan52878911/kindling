package daemon

// POST /sandboxes/{ref}/fork: ramificar un sandbox vivo en N copias. El trabajo
// lo hace machine.Fork (ver internal/machine/fork.go); aquí se valida la
// petición con las mismas reglas que la creación de un sandbox y se espera al
// agente de cada copia, como handleCreateSandbox.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

// forkAgentWait es cuánto se espera al agente de cada copia. Una copia
// restaurada lo trae ya escuchando en la memoria congelada, así que lo normal
// son milisegundos; el margen es el mismo que al crear un sandbox, por el
// mismo motivo (un host cargado).
const forkAgentWait = 2 * time.Minute

// forkStatus traduce los errores de machine.Fork a códigos HTTP.
func forkStatus(err error) int {
	switch {
	case errors.Is(err, machine.ErrNoMachine):
		return http.StatusNotFound
	case errors.Is(err, machine.ErrNotRunning), errors.Is(err, machine.ErrFork), errors.Is(err, machine.ErrNotReady),
		errors.Is(err, machine.ErrSharesCommit), errors.Is(err, machine.ErrExecNotInSnapshot):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// validarFork aplica a la petición las reglas de POST /sandboxes: el mismo tope
// de TTL y los mismos valores de on_ttl. Los ceros significan "los del
// original" y los resuelve machine.Fork.
func validarFork(req api.ForkRequest) error {
	if req.Count < 0 || req.Count > api.ForkMax {
		return fmt.Errorf("count must be between 1 and %d", api.ForkMax)
	}
	if req.TTLSeconds < 0 || req.TTLSeconds > api.SandboxMaxTTL {
		return fmt.Errorf("ttl_seconds must be between 1 and %d", api.SandboxMaxTTL)
	}
	switch req.OnTTL {
	case "", api.OnTTLRemove, api.OnTTLFreeze:
	default:
		return fmt.Errorf("invalid on_ttl %q: use %q or %q", req.OnTTL, api.OnTTLRemove, api.OnTTLFreeze)
	}
	if err := api.ValidateForkLabels(req.Labels); err != nil {
		return err
	}
	if req.ReadyTimeoutSeconds < 0 || req.ReadyTimeoutSeconds > api.ReadyMaxWaitSeconds {
		return fmt.Errorf("ready_timeout_seconds must be between 0 and %d", api.ReadyMaxWaitSeconds)
	}
	return nil
}

func (s *Server) handleForkSandbox(w http.ResponseWriter, r *http.Request) {
	src, ok := s.sandbox(w, r.PathValue("ref"))
	if !ok {
		return
	}
	var req api.ForkRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			fail(w, jsonBodyStatus(err), err)
			return
		}
	}
	if err := validarFork(req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	espera := time.Duration(req.ReadyTimeoutSeconds) * time.Second
	snap, forks, err := s.mgr.Fork(r.Context(), src.ID, machine.ForkOptions{
		Count: req.Count, TTLSeconds: req.TTLSeconds, OnTTL: req.OnTTL, Labels: req.Labels,
		SkipReady: req.SkipReady, ReadyWait: espera,
		// Como al crear un sandbox: se devuelven cuando el agente ya escucha,
		// porque quien las pide va a ejecutar algo en ellas acto seguido. Y,
		// si la imagen declara sonda o ganchos, cuando está lista: una copia
		// cuyos ganchos (su identidad) no han terminado no es aún la suya.
		Lista: func(ctx context.Context, mc *api.Machine) error {
			if err := s.esperarPuerto(ctx, mc, api.GuestPort, forkAgentWait); err != nil {
				return fmt.Errorf("the guest agent of %s never answered in %s: %w", mc.Name, forkAgentWait, err)
			}
			if req.SkipReady {
				return nil
			}
			if _, err := s.mgr.WaitReady(ctx, mc.ID, machine.OpcionesListo{Plazo: espera, SinAgenteVale: true}); err != nil {
				return err
			}
			return nil
		},
	})
	if err != nil {
		fail(w, forkStatus(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, api.ForkResult{Snapshot: snap, Sandboxes: forks})
}
