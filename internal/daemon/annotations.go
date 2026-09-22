package daemon

import (
	"net/http"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
)

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := s.mgr.Snapshot(r.PathValue("name"))
	if err != nil {
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "does not exist") {
			code = http.StatusNotFound
		}
		fail(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleSetAnnotation(w http.ResponseWriter, r *http.Request) {
	b, err := api.LeerCuerpo(r.Body, api.MaxValueBytes)
	if err != nil {
		fail(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	snap, err := s.mgr.SetAnnotation(r.PathValue("name"), r.PathValue("key"), b)
	if err != nil {
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "does not exist") {
			code = http.StatusNotFound
		}
		fail(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleRemoveAnnotation(w http.ResponseWriter, r *http.Request) {
	if _, err := s.mgr.RemoveAnnotation(r.PathValue("name"), r.PathValue("key")); err != nil {
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "does not exist") {
			code = http.StatusNotFound
		}
		fail(w, code, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
