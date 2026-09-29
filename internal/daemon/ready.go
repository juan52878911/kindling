package daemon

// GET /machines/{ref}/ready y POST /machines/{ref}/hooks: el "listo" que define
// la imagen y sus ganchos tras restaurar (internal/machine/listo.go).

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

// esperaDeQuery lee ?wait= (duración de Go, o segundos a secas), acotado.
func esperaDeQuery(r *http.Request) (time.Duration, error) {
	v := r.URL.Query().Get("wait")
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		var n int
		if _, serr := fmt.Sscanf(v, "%d", &n); serr != nil || fmt.Sprint(n) != v {
			return 0, fmt.Errorf("invalid wait %q: use a duration like 30s", v)
		}
		d = time.Duration(n) * time.Second
	}
	if d < 0 || d > api.ReadyMaxWaitSeconds*time.Second {
		return 0, fmt.Errorf("wait must be between 0 and %ds", api.ReadyMaxWaitSeconds)
	}
	return d, nil
}

// estadoDeListo traduce los errores de la espera. ErrNotReady no es un error
// HTTP: la respuesta (ReadyResult) ya dice que no está listo.
func estadoDeListo(err error) int {
	switch {
	case errors.Is(err, machine.ErrNoMachine):
		return http.StatusNotFound
	case errors.Is(err, machine.ErrNotRunning):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	espera, err := esperaDeQuery(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	o := machine.OpcionesListo{Plazo: espera, UnaVez: espera == 0}
	res, err := s.mgr.WaitReady(r.Context(), r.PathValue("ref"), o)
	if err != nil && !errors.Is(err, machine.ErrNotReady) {
		fail(w, estadoDeListo(err), err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleHooks(w http.ResponseWriter, r *http.Request) {
	espera, err := esperaDeQuery(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.mgr.RunHooks(r.Context(), r.PathValue("ref"), espera)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, res)
	case errors.Is(err, machine.ErrNotReady) && res.ID != "" && res.Guest != nil:
		// Corrieron y fallaron (o no acabaron a tiempo): el resultado lo dice.
		writeJSON(w, http.StatusOK, res)
	case errors.Is(err, machine.ErrNotReady):
		fail(w, http.StatusConflict, err)
	default:
		fail(w, estadoDeListo(err), err)
	}
}
