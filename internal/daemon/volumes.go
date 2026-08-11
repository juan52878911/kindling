package daemon

import (
	"encoding/json"
	"net/http"

	"github.com/juan52878911/kindling/internal/api"
)

func (s *Server) handleVolumes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.Volumes())
}

func (s *Server) handleCreateVolume(w http.ResponseWriter, r *http.Request) {
	var req api.CreateVolumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	// Formatear un ext4 tarda; se acota por el contexto de la petición porque
	// aquí no queda nada a medias que pueda envenenar al host: CreateVolume
	// construye en .tmp y solo renombra al terminar.
	v, err := s.mgr.CreateVolume(r.Context(), req.Name, req.SizeMiB)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleRemoveVolume(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.RemoveVolume(r.PathValue("name")); err != nil {
		// 409 y no 400: "lo está usando alguien" es un conflicto de estado, no
		// una petición mal formada, y quien llama puede reintentar más tarde.
		fail(w, http.StatusConflict, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
