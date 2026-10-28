package daemon

import (
	"errors"
	"net/http"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

// snapshotStatus traduce un error de snapshot a su código HTTP con las
// centinelas de machine: 404 si no existe, 409 si está en uso y 400 para el
// resto (nombre inválido, meta de un kling más nuevo...).
func snapshotStatus(err error) int {
	switch {
	case errors.Is(err, machine.ErrNoSnapshot):
		return http.StatusNotFound
	case errors.Is(err, machine.ErrSnapshotInUse):
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := s.mgr.Snapshot(r.PathValue("name"))
	if err != nil {
		fail(w, snapshotStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleSetAnnotation(w http.ResponseWriter, r *http.Request) {
	b, err := api.LeerCuerpo(r.Body, api.MaxValueBytes)
	if err != nil {
		fail(w, jsonBodyStatus(err), err)
		return
	}
	snap, err := s.mgr.SetAnnotation(r.PathValue("name"), r.PathValue("key"), b)
	if err != nil {
		fail(w, snapshotStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleRemoveAnnotation(w http.ResponseWriter, r *http.Request) {
	if _, err := s.mgr.RemoveAnnotation(r.PathValue("name"), r.PathValue("key")); err != nil {
		fail(w, snapshotStatus(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
